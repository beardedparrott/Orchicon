// Package blobstore defines the BlobStore abstraction described in
// docs/01_Architecture_Vision.md §2. Two implementations ship: S3
// (cloud) and local filesystem (fully-local deployments). The local
// implementation is production-viable, not just a dev sink.
package blobstore

import (
	"context"
	"io"
)

// Blob is a stored object reference.
type Blob struct {
	Ref      string // opaque storage key
	MimeType string
	Size     int64
	SHA256   string
}

// Store is the object-storage abstraction. Implementations must be
// safe for concurrent use.
type Store interface {
	// Put stores r under ref and returns metadata.
	Put(ctx context.Context, ref string, mimeType string, r io.Reader) (Blob, error)
	// Get returns a reader for ref. The caller closes the reader.
	Get(ctx context.Context, ref string) (io.ReadCloser, error)
	// Delete removes ref. Missing refs are not errors.
	Delete(ctx context.Context, ref string) error
}
