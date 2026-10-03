package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/joho/godotenv"

	"github.com/tanveer-shaikh-90/seat-reservation/internal/api"
	"github.com/tanveer-shaikh-90/seat-reservation/internal/db"
	"github.com/tanveer-shaikh-90/seat-reservation/internal/service"
)

func main() {
	_ = godotenv.Load() // optional .env for local dev; ignored in prod

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	dsn := env("DATABASE_URL", "postgres://postgres:postgres@localhost:5432/seats?sslmode=disable")
	port := env("PORT", "8080")
	maxConns := envInt("DB_MAX_CONNS", 25)
	if maxConns < 1 || maxConns > 100 {
		logger.Error("DB_MAX_CONNS must be between 1 and 100")
		os.Exit(1)
	}
	auth, err := api.NewAuthenticator(os.Getenv("AUTH_SECRET"), os.Getenv("ADMIN_TOKEN"))
	if err != nil {
		logger.Error("invalid auth configuration", "error", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := db.New(ctx, dsn, int32(maxConns))
	if err != nil {
		logger.Error("failed to connect to database", "error", err)
		os.Exit(1)
	}
	defer pool.Close()

	if err := pool.Migrate(ctx); err != nil {
		logger.Error("failed to run migrations", "error", err)
		os.Exit(1)
	}
	logger.Info("migrations applied")

	svc := service.New(pool)
	srv := api.NewServer(svc, pool, logger, auth)

	httpServer := &http.Server{
		Addr:              ":" + port,
		Handler:           srv.Router(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      190 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}

	go func() {
		logger.Info("server listening", "port", port)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("server error", "error", err)
			stop()
		}
	}()

	<-ctx.Done()
	logger.Info("shutting down")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		logger.Error("graceful shutdown failed", "error", err)
	}
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envInt(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}
