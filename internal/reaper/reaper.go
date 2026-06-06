// Package reaper runs periodic background cleanup of stale data.
package reaper

import (
	"context"
	"time"

	"github.com/aarani/craftling-go/internal/repository"
	"go.uber.org/zap"
)

// RefreshTokens periodically deletes expired refresh tokens until ctx is
// cancelled. It is intended to run in its own goroutine.
func RefreshTokens(ctx context.Context, log *zap.Logger, repo *repository.RefreshTokenRepository, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			reap(ctx, log, repo)
		}
	}
}

func reap(ctx context.Context, log *zap.Logger, repo *repository.RefreshTokenRepository) {
	opCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	n, err := repo.DeleteExpired(opCtx)
	if err != nil {
		log.Error("reap expired refresh tokens", zap.Error(err))
		return
	}
	if n > 0 {
		log.Info("reaped expired refresh tokens", zap.Int64("count", n))
	}
}

// Hosts periodically marks fleet hosts down once their last heartbeat is older
// than ttl. It runs in its own goroutine until ctx is cancelled. This is the
// liveness half of P1's control-plane-authoritative model: agents push
// heartbeats up; a host that goes quiet is presumed unreachable.
func Hosts(ctx context.Context, log *zap.Logger, repo *repository.HostRepository, interval, ttl time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			reapHosts(ctx, log, repo, ttl)
		}
	}
}

func reapHosts(ctx context.Context, log *zap.Logger, repo *repository.HostRepository, ttl time.Duration) {
	n, err := repo.MarkStale(ctx, time.Now().Add(-ttl))
	if err != nil {
		log.Error("mark stale hosts down", zap.Error(err))
		return
	}
	if n > 0 {
		log.Warn("marked stale hosts down", zap.Int("count", n))
	}
}
