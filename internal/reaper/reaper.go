// Package reaper runs periodic background cleanup of stale data.
package reaper

import (
	"context"
	"time"

	"github.com/aarani/craftling-go/internal/repository"
	"github.com/aarani/craftling-go/internal/storage"
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

// activeServerLister returns the ids of every live (non-deleted) server. The
// GameServerRepository satisfies it; an interface keeps the world reaper unit
// testable without a database.
type activeServerLister interface {
	ListActiveIDs(ctx context.Context) ([]string, error)
}

// Worlds periodically deletes stored world snapshots that no live server claims
// (P5). The agent removes a world on deprovision in the happy path; this catches
// orphans — worlds left behind when the assigned host was permanently gone at
// delete time, or rows force-removed from the database. It runs in its own
// goroutine until ctx is cancelled.
func Worlds(ctx context.Context, log *zap.Logger, store storage.WorldStore, servers activeServerLister, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			reapWorlds(ctx, log, store, servers)
		}
	}
}

func reapWorlds(ctx context.Context, log *zap.Logger, store storage.WorldStore, servers activeServerLister) {
	opCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	stored, err := store.List(opCtx)
	if err != nil {
		log.Error("list stored worlds", zap.Error(err))
		return
	}
	if len(stored) == 0 {
		return
	}

	// Listing live servers must succeed before we delete anything: a transient
	// DB error must never be read as "no servers exist" and wipe every world.
	ids, err := servers.ListActiveIDs(opCtx)
	if err != nil {
		log.Error("list active servers for world GC", zap.Error(err))
		return
	}
	live := make(map[string]bool, len(ids))
	for _, id := range ids {
		live[storage.SafeKey(id)] = true // stored keys are already SafeKey'd
	}

	var removed int
	for _, key := range stored {
		if live[key] {
			continue
		}
		if err := store.Delete(opCtx, key); err != nil {
			log.Warn("delete orphan world", zap.String("key", key), zap.Error(err))
			continue
		}
		removed++
	}
	if removed > 0 {
		log.Info("reaped orphan worlds", zap.Int("count", removed))
	}
}
