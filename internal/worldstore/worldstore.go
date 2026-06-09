// Package worldstore selects and constructs the durable WorldStore (P5b) from
// config. It lives apart from internal/storage so that importing the storage
// interface (as the Firecracker driver does) does not pull in the S3 SDK; only
// the binaries that actually build a store — cmd/agent and cmd/server (for world
// GC) — import this package and link the SDK.
package worldstore

import (
	"context"

	"github.com/aarani/craftling-go/internal/config"
	"github.com/aarani/craftling-go/internal/storage"
	s3store "github.com/aarani/craftling-go/internal/storage/s3"
	"go.uber.org/zap"
)

// FromConfig builds the durable world store described by the Firecracker config:
// an S3-compatible bucket when an endpoint is set, otherwise a local/NFS
// directory, otherwise none (nil, nil — worlds stay host-local). S3 takes
// precedence over a directory. The returned store, log line, and any error
// match for both the agent (snapshot/restore) and the control plane (GC), so
// they always agree on where worlds live.
func FromConfig(ctx context.Context, fc config.FirecrackerConfig, log *zap.Logger) (storage.WorldStore, error) {
	if fc.WorldStoreS3.Endpoint != "" {
		s, err := s3store.New(ctx, s3store.Config{
			Endpoint:        fc.WorldStoreS3.Endpoint,
			Bucket:          fc.WorldStoreS3.Bucket,
			Region:          fc.WorldStoreS3.Region,
			AccessKeyID:     fc.WorldStoreS3.AccessKeyID,
			SecretAccessKey: fc.WorldStoreS3.SecretAccessKey,
			UseSSL:          fc.WorldStoreS3.UseSSL,
			Prefix:          fc.WorldStoreS3.Prefix,
		})
		if err != nil {
			return nil, err
		}
		log.Info("world store enabled",
			zap.String("backend", "s3"),
			zap.String("endpoint", fc.WorldStoreS3.Endpoint),
			zap.String("bucket", fc.WorldStoreS3.Bucket))
		return s, nil
	}
	if dir := fc.WorldStoreDir; dir != "" {
		s, err := storage.NewDirStore(dir)
		if err != nil {
			return nil, err
		}
		log.Info("world store enabled", zap.String("backend", "dir"), zap.String("dir", dir))
		return s, nil
	}
	return nil, nil
}
