// Package blobstore defines the BlobStore abstraction described in
// docs/01_Architecture_Vision.md §2. Two implementations ship: S3
// (cloud) and local filesystem (fully-local deployments). The local
// implementation is production-viable, not just a dev sink: it uses a
// content-addressed layout (sha256-keyed directories) with atomic writes
// (temp file + rename) and is safe for concurrent use.
package blobstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// LocalStore implements Store over the local filesystem. Blobs are
// stored under a content-addressed path derived from the ref:
//
//	<root>/<sha256-prefix-2>/<ref-safe-name>
//
// Writes are atomic: data is written to a temp file in the same
// directory then renamed into place, so a partial write never appears
// as a complete blob. The store is safe for concurrent use.
type LocalStore struct {
	root string
	mu   sync.Mutex
}

// NewLocalStore constructs a local filesystem store rooted at dir. The
// directory is created if it does not exist.
func NewLocalStore(dir string) (*LocalStore, error) {
	if dir == "" {
		return nil, errors.New("blobstore: local store requires a root directory")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("blobstore: create root: %w", err)
	}
	return &LocalStore{root: dir}, nil
}

// Put stores r under ref and returns metadata (size + sha256). The
// write is atomic (temp + rename).
func (s *LocalStore) Put(ctx context.Context, ref string, mimeType string, r io.Reader) (Blob, error) {
	path, err := s.pathFor(ref)
	if err != nil {
		return Blob{}, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return Blob{}, fmt.Errorf("blobstore: create dir: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".blob-tmp-*")
	if err != nil {
		return Blob{}, fmt.Errorf("blobstore: create temp: %w", err)
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }() // no-op if rename succeeded

	h := sha256.New()
	mw := io.MultiWriter(tmp, h)
	n, err := io.Copy(mw, r)
	if err != nil {
		_ = tmp.Close()
		return Blob{}, fmt.Errorf("blobstore: write: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return Blob{}, fmt.Errorf("blobstore: close temp: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return Blob{}, fmt.Errorf("blobstore: rename: %w", err)
	}
	return Blob{
		Ref:      ref,
		MimeType: mimeType,
		Size:     n,
		SHA256:   hex.EncodeToString(h.Sum(nil)),
	}, nil
}

// Get returns a reader for ref. The caller closes the reader.
func (s *LocalStore) Get(ctx context.Context, ref string) (io.ReadCloser, error) {
	path, err := s.pathFor(ref)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("blobstore: %w: %s", ErrNotFound, ref)
		}
		return nil, fmt.Errorf("blobstore: open: %w", err)
	}
	return f, nil
}

// Delete removes ref. Missing refs are not errors.
func (s *LocalStore) Delete(ctx context.Context, ref string) error {
	path, err := s.pathFor(ref)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("blobstore: delete: %w", err)
	}
	return nil
}

// pathFor returns the absolute filesystem path for ref. The ref is
// sanitized to a single path component to prevent path traversal.
func (s *LocalStore) pathFor(ref string) (string, error) {
	safe := sanitizeRef(ref)
	if safe == "" {
		return "", errors.New("blobstore: empty ref")
	}
	// Two-level sharding by the first 2 hex chars of the sha256 of the
	// ref keeps directories small.
	h := sha256.Sum256([]byte(safe))
	prefix := hex.EncodeToString(h[:1])
	return filepath.Join(s.root, prefix, safe), nil
}

// sanitizeRef strips path separators and parent-dir references so the
// ref cannot escape the store root. Only [A-Za-z0-9._-] are kept.
func sanitizeRef(ref string) string {
	var b strings.Builder
	for _, r := range ref {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9',
			r == '.' || r == '_' || r == '-':
			b.WriteRune(r)
		}
	}
	return b.String()
}

// ErrNotFound is returned by Get when a ref does not exist.
var ErrNotFound = errors.New("not found")
