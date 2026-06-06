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
	"github.com/aarani/craftling-go/internal/model"
	"github.com/aarani/craftling-go/internal/reaper"
	"github.com/aarani/craftling-go/internal/reconciler"
	"github.com/aarani/craftling-go/internal/repository"
	"github.com/aarani/craftling-go/internal/scheduler"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
	"go.uber.org/zap"
)

// Shared test fixtures, set up in TestMain.
var (
	baseURL string        // address of the test HTTP server
	pool    *pgxpool.Pool // direct DB access for integration assertions
)

// Host-reaper timing for tests: short so the stale->down transition is quick.
const (
	hostHeartbeatTTL = 200 * time.Millisecond
	hostReapInterval = 25 * time.Millisecond
)

// Capacity of the always-on placement host registered in TestMain. It is sized
// large in cpu so every test's server can be placed (the scheduler needs a ready
// host), but its memory total is deliberately below the maximum allowed server
// spec so a create request can still exceed it and exercise the oversize path.
const (
	placementHostCPUs     = 64
	placementHostMemoryMB = 32768
)

// placementHostID is the id of the kept-alive host the reconciler schedules onto
// in e2e. Set in TestMain.
var placementHostID string

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
	hostRepo := repository.NewHostRepository()
	srv := httptest.NewServer(handler.NewRouter(cfg, zap.NewNop(), pool, hostRepo))
	baseURL = srv.URL

	// Run the reconciler with a fast tick so lifecycle tests converge quickly.
	recCtx, recCancel := context.WithCancel(ctx)
	sched := scheduler.New(hostRepo)
	rec := reconciler.New(repository.NewGameServerRepository(pool), provisioner.NewFake(), sched, zap.NewNop())
	go rec.Run(recCtx, 100*time.Millisecond)

	// Run the host reaper with short timing so the stale->down test converges.
	go reaper.Hosts(recCtx, zap.NewNop(), hostRepo, hostReapInterval, hostHeartbeatTTL)

	// Register an always-on host so the scheduler has somewhere to place servers,
	// and keep it alive against the reaper's short TTL. Heartbeating (not
	// re-registering) preserves its allocatable capacity so reservation
	// accounting stays observable across the suite.
	placed, err := hostRepo.Register(ctx, &model.Host{
		Hostname:      "placement-host",
		Address:       "10.0.0.100:9000",
		Zone:          "zone-a",
		CPUsTotal:     placementHostCPUs,
		MemoryMBTotal: placementHostMemoryMB,
		AgentVersion:  "test",
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "register placement host: %v\n", err)
		os.Exit(1)
	}
	placementHostID = placed.ID
	go keepHostAlive(recCtx, hostRepo, placementHostID)

	code := m.Run()

	recCancel()
	srv.Close()
	pool.Close()
	_ = pg.Terminate(ctx)
	os.Exit(code)
}

// keepHostAlive heartbeats a host well within the reaper TTL so it stays ready
// for the whole suite, without re-registering (which would reset its capacity).
func keepHostAlive(ctx context.Context, repo *repository.HostRepository, id string) {
	ticker := time.NewTicker(hostHeartbeatTTL / 4)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_ = repo.Heartbeat(ctx, id)
		}
	}
}
