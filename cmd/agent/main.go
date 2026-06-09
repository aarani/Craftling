// Command agent is the host-side worker (P3). It exposes a VM API the control
// plane calls to provision/start/stop/deprovision local VMs, and it registers +
// heartbeats with the control plane so the scheduler can place servers on it.
//
// It ships with the in-memory FakeRuntime; a real Firecracker driver (P4) slots
// in behind the same Runtime interface without changing this wiring.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os/signal"
	"syscall"
	"time"

	"github.com/aarani/craftling-go/internal/agent"
	"github.com/aarani/craftling-go/internal/agent/firecracker"
	"github.com/aarani/craftling-go/internal/config"
	applogger "github.com/aarani/craftling-go/internal/logger"
	"github.com/aarani/craftling-go/internal/worldstore"
	"go.uber.org/zap"
)

const (
	// heartbeatInterval is how often the agent proves liveness to the control
	// plane. It must be comfortably below the control plane's host TTL (30s).
	heartbeatInterval = 10 * time.Second
	// registerRetryInterval is how long to wait between registration attempts
	// while the control plane is unreachable.
	registerRetryInterval = 5 * time.Second
)

func main() {
	cfg := config.Load()

	zlog, err := applogger.New(cfg.Env)
	if err != nil {
		log.Fatalf("init logger: %v", err)
	}
	defer func() { _ = zlog.Sync() }()

	advertiseAddr := cfg.Agent.AdvertiseAddr
	if advertiseAddr == "" {
		// Best-effort default so a local single-host run works out of the box.
		advertiseAddr = "localhost:" + cfg.Port
	}

	// The runtime that actually runs VMs, fronted by the agent HTTP API.
	rt, err := newRuntime(cfg, zlog)
	if err != nil {
		zlog.Fatal("init runtime", zap.Error(err))
	}
	srv := &http.Server{
		Addr:         ":" + cfg.Port,
		Handler:      agent.NewRouter(rt, zlog),
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go func() {
		zlog.Info("agent listening",
			zap.String("port", cfg.Port), zap.String("advertise_addr", advertiseAddr))
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			zlog.Fatal("agent listen failed", zap.Error(err))
		}
	}()

	// Register with the control plane and keep the host alive via heartbeats.
	cp := agent.NewCPClient(cfg.Agent.ControlPlaneURL, &http.Client{Timeout: 10 * time.Second})
	go runRegistration(ctx, zlog, cp, agent.RegisterRequest{
		ID:            cfg.Agent.ID,
		Hostname:      cfg.Agent.Hostname,
		Address:       advertiseAddr,
		Zone:          cfg.Agent.Zone,
		CPUsTotal:     cfg.Agent.CPUsTotal,
		MemoryMBTotal: cfg.Agent.MemoryMBTotal,
		AgentVersion:  cfg.Agent.Version,
	})

	<-ctx.Done()
	stop()
	zlog.Info("shutting down agent...")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		zlog.Fatal("forced shutdown", zap.Error(err))
	}
	zlog.Info("agent exited")
}

// newRuntime selects the VM backend by config: the in-memory FakeRuntime for
// local/dev runs, or the real Firecracker driver (P4) on KVM hosts.
func newRuntime(cfg *config.Config, log *zap.Logger) (agent.Runtime, error) {
	switch cfg.Agent.Runtime {
	case config.RuntimeFirecracker:
		log.Info("using firecracker runtime",
			zap.String("kernel", cfg.Agent.Firecracker.KernelPath),
			zap.String("image_dir", cfg.Agent.Firecracker.ImageDir))
		storeCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		worldStore, err := worldstore.FromConfig(storeCtx, cfg.Agent.Firecracker, log)
		cancel()
		if err != nil {
			return nil, err
		}
		return firecracker.New(firecracker.Config{
			BinaryPath:       cfg.Agent.Firecracker.BinaryPath,
			KernelPath:       cfg.Agent.Firecracker.KernelPath,
			ImageDir:         cfg.Agent.Firecracker.ImageDir,
			DefaultImage:     cfg.Agent.Firecracker.DefaultImage,
			WorkDir:          cfg.Agent.Firecracker.WorkDir,
			AdvertiseHost:    cfg.Agent.AdvertiseHost,
			WorldPersistence: cfg.Agent.Firecracker.WorldPersistence,
			DataDir:          cfg.Agent.Firecracker.DataDir,
			WorldDiskMB:      cfg.Agent.Firecracker.WorldDiskMB,
			MkfsExt4Path:     cfg.Agent.Firecracker.MkfsExt4Path,
			WorldStore:       worldStore,
			SnapshotInterval: cfg.Agent.Firecracker.SnapshotInterval,
			RCONPort:         cfg.Agent.Firecracker.RCONPort,
			RCONPassword:     cfg.Agent.Firecracker.RCONPassword,
			Logger:           log,
		})
	case config.RuntimeFake, "":
		log.Info("using fake runtime")
		return agent.NewFakeRuntime(cfg.Agent.AdvertiseHost), nil
	default:
		return nil, fmt.Errorf("unknown agent runtime %q", cfg.Agent.Runtime)
	}
}

// runRegistration registers the host then heartbeats on an interval until ctx is
// cancelled. A heartbeat that the control plane rejects with "not found" (it was
// restarted and forgot us) triggers a re-register, restoring the same identity.
func runRegistration(ctx context.Context, log *zap.Logger, cp *agent.CPClient, req agent.RegisterRequest) {
	id := register(ctx, log, cp, req)
	if id == "" {
		return // ctx cancelled before we registered
	}
	// Re-register under the assigned id so identity stays stable across restarts.
	req.ID = id

	ticker := time.NewTicker(heartbeatInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			found, err := cp.Heartbeat(ctx, id)
			if err != nil {
				log.Warn("heartbeat failed", zap.Error(err))
				continue
			}
			if !found {
				log.Warn("control plane forgot host; re-registering", zap.String("id", id))
				if newID := register(ctx, log, cp, req); newID != "" {
					id = newID
					req.ID = newID
				}
			}
		}
	}
}

// register retries registration until it succeeds or ctx is cancelled,
// returning the assigned host id (empty on cancellation).
func register(ctx context.Context, log *zap.Logger, cp *agent.CPClient, req agent.RegisterRequest) string {
	for {
		id, err := cp.Register(ctx, req)
		if err == nil {
			log.Info("registered with control plane", zap.String("id", id))
			return id
		}
		log.Warn("register failed; retrying", zap.Error(err))

		select {
		case <-ctx.Done():
			return ""
		case <-time.After(registerRetryInterval):
		}
	}
}
