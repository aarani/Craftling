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
