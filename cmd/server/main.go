package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os/signal"
	"syscall"
	"time"

	"github.com/aarani/craftling-go/internal/config"
	"github.com/aarani/craftling-go/internal/db"
	"github.com/aarani/craftling-go/internal/handler"
	applogger "github.com/aarani/craftling-go/internal/logger"
	"github.com/aarani/craftling-go/internal/provisioner"
	"github.com/aarani/craftling-go/internal/reaper"
	"github.com/aarani/craftling-go/internal/reconciler"
	"github.com/aarani/craftling-go/internal/repository"
	"github.com/aarani/craftling-go/internal/scheduler"
	"github.com/aarani/craftling-go/internal/seed"
	"go.uber.org/zap"
)

const (
	// reapInterval is how often expired refresh tokens are purged.
	reapInterval = time.Hour
	// reconcileInterval is how often game servers are reconciled.
	reconcileInterval = 2 * time.Second
	// hostReapInterval is how often the fleet is swept for stale hosts.
	hostReapInterval = 10 * time.Second
	// hostHeartbeatTTL is how long a host may go without heartbeating before it
	// is marked down.
	hostHeartbeatTTL = 30 * time.Second
)

func main() {
	cfg := config.Load()

	zlog, err := applogger.New(cfg.Env)
	if err != nil {
		log.Fatalf("init logger: %v", err)
	}
	defer func() { _ = zlog.Sync() }()

	// Connect to Postgres and apply the schema.
	dbCtx, dbCancel := context.WithTimeout(context.Background(), 10*time.Second)
	pool, err := db.Connect(dbCtx, cfg.DatabaseURL)
	if err != nil {
		dbCancel()
		zlog.Fatal("connect to database", zap.Error(err))
	}
	defer pool.Close()
	if err := db.Migrate(dbCtx, pool); err != nil {
		dbCancel()
		zlog.Fatal("run migrations", zap.Error(err))
	}

	// Optionally bootstrap the admin account.
	if created, err := seed.Admin(dbCtx, repository.NewUserRepository(pool), cfg.AdminEmail, cfg.AdminPassword); err != nil {
		dbCancel()
		zlog.Fatal("seed admin", zap.Error(err))
	} else if created {
		zlog.Info("seeded admin user", zap.String("email", cfg.AdminEmail))
	}
	dbCancel()

	// The fleet inventory lives in process memory (P1). It is shared between the
	// HTTP handlers (register/heartbeat) and the host reaper.
	hostRepo := repository.NewHostRepository()

	router := handler.NewRouter(cfg, zlog, pool, hostRepo)

	srv := &http.Server{
		Addr:         ":" + cfg.Port,
		Handler:      router,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	// ctx is cancelled on the first interrupt/terminate signal, which both
	// stops the reaper and triggers graceful shutdown.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Periodically purge expired refresh tokens.
	go reaper.RefreshTokens(ctx, zlog, repository.NewRefreshTokenRepository(pool), reapInterval)

	// Periodically mark hosts down once their heartbeats go stale.
	go reaper.Hosts(ctx, zlog, hostRepo, hostReapInterval, hostHeartbeatTTL)

	// Continuously reconcile game servers toward their desired state. The
	// scheduler places unassigned servers onto ready hosts from the same fleet
	// inventory the agent endpoints and host reaper share.
	sched := scheduler.New(hostRepo)
	rec := reconciler.New(repository.NewGameServerRepository(pool), provisioner.NewFake(), sched, zlog)
	go rec.Run(ctx, reconcileInterval)

	// Start the server in a goroutine so it doesn't block graceful shutdown handling.
	go func() {
		zlog.Info("server listening", zap.String("port", cfg.Port), zap.String("env", cfg.Env))
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			zlog.Fatal("listen failed", zap.Error(err))
		}
	}()

	<-ctx.Done()
	stop() // restore default signal handling so a second signal force-quits
	zlog.Info("shutting down server...")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		zlog.Fatal("forced shutdown", zap.Error(err))
	}

	zlog.Info("server exited")
}
