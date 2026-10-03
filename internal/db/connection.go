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
	if _, err := p.Exec(ctx, migrationSQL); err != nil {
		return fmt.Errorf("apply migrations: %w", err)
	}
	return nil
}

// Ready reports whether the database dependency is reachable. Used by the
// readiness probe so the service fails closed when Postgres is down.
func (p *Pool) Ready(ctx context.Context) error {
	pingCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	return p.Ping(pingCtx)
}
