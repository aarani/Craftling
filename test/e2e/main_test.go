//go:build e2e

// Package e2e contains end-to-end tests that exercise the real HTTP server
// against a real PostgreSQL instance started via testcontainers.
//
// Run with: go test -tags e2e ./test/e2e/...  (requires Docker).
package e2e

import (
	"context"
	"fmt"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/aarani/craftling-go/internal/config"
	"github.com/aarani/craftling-go/internal/db"
	"github.com/aarani/craftling-go/internal/handler"
	"github.com/aarani/craftling-go/internal/provisioner"
	"github.com/aarani/craftling-go/internal/reconciler"
	"github.com/aarani/craftling-go/internal/repository"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
	"go.uber.org/zap"
)

// Shared test fixtures, set up in TestMain.
var (
	baseURL string         // address of the test HTTP server
	pool    *pgxpool.Pool  // direct DB access for integration assertions
)

func TestMain(m *testing.M) {
	ctx := context.Background()

	pg, err := postgres.Run(ctx, "postgres:16-alpine",
		postgres.WithDatabase("craftling"),
		postgres.WithUsername("postgres"),
		postgres.WithPassword("postgres"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(60*time.Second),
		),
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "start postgres container: %v\n", err)
		os.Exit(1)
	}

	connStr, err := pg.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		fmt.Fprintf(os.Stderr, "connection string: %v\n", err)
		os.Exit(1)
	}

	pool, err = db.Connect(ctx, connStr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "connect db: %v\n", err)
		os.Exit(1)
	}
	if err := db.Migrate(ctx, pool); err != nil {
		fmt.Fprintf(os.Stderr, "migrate: %v\n", err)
		os.Exit(1)
	}

	cfg := &config.Config{
		Env:        "test",
		JWTSecret:  "test-secret",
		AccessTTL:  time.Hour,
		RefreshTTL: time.Hour,
	}
	srv := httptest.NewServer(handler.NewRouter(cfg, zap.NewNop(), pool))
	baseURL = srv.URL

	// Run the reconciler with a fast tick so lifecycle tests converge quickly.
	recCtx, recCancel := context.WithCancel(ctx)
	rec := reconciler.New(repository.NewGameServerRepository(pool), provisioner.NewFake(), zap.NewNop())
	go rec.Run(recCtx, 100*time.Millisecond)

	code := m.Run()

	recCancel()
	srv.Close()
	pool.Close()
	_ = pg.Terminate(ctx)
	os.Exit(code)
}
