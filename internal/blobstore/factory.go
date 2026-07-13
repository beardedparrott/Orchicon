package blobstore

import (
	"context"
	"fmt"

	"github.com/beardedparrott/orchicon/internal/config"
)

// New constructs the BlobStore selected by BlobStoreConfig. "local" uses
// the production-viable filesystem store; "s3" uses S3-compatible
// storage.
func New(ctx context.Context, cfg config.BlobStoreConfig) (Store, error) {
	switch cfg.Kind {
	case "local", "":
		return NewLocalStore(cfg.LocalDir)
	case "s3":
		return NewS3Store(ctx, cfg.S3Bucket, cfg.S3Region, cfg.S3Endpoint)
	default:
		return nil, fmt.Errorf("blobstore: unknown kind %q", cfg.Kind)
	}
}
