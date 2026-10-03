package db

import (
	"context"
	_ "embed"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations.sql
var migrationSQL string

// Pool wraps the pgx connection pool.
type Pool struct {
	*pgxpool.Pool
}

// New creates a bounded connection pool. MaxConns is kept deliberately small so
// contention is resolved by Postgres row locks instead of exhausting the DB.
func New(ctx context.Context, dsn string, maxConns int32) (*Pool, error) {
	if maxConns < 1 || maxConns > 100 {
		return nil, fmt.Errorf("maxConns must be between 1 and 100")
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse dsn: %w", err)
	}
	cfg.MaxConns = maxConns
	cfg.MaxConnLifetime = time.Hour
	cfg.HealthCheckPeriod = 30 * time.Second

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("create pool: %w", err)
	}

	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping db: %w", err)
	}
	return &Pool{pool}, nil
}

// Migrate applies the embedded schema. It is idempotent.
func (p *Pool) Migrate(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	tx, err := p.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(730021)`); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (version INT PRIMARY KEY)`); err != nil {
		return err
	}
	var applied bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE version=1)`).Scan(&applied); err != nil {
		return err
	}
	if applied {
		return tx.Commit(ctx)
	}
	if _, err := tx.Exec(ctx, migrationSQL); err != nil {
		return fmt.Errorf("apply migrations: %w", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO schema_migrations (version) VALUES (1)`); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Ready reports whether the database dependency is reachable. Used by the
// readiness probe so the service fails closed when Postgres is down.
func (p *Pool) Ready(ctx context.Context) error {
	pingCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	return p.Ping(pingCtx)
}
