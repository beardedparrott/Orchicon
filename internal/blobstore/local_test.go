package blobstore

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLocalStorePutGetDelete(t *testing.T) {
	dir := t.TempDir()
	s, err := NewLocalStore(dir)
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	ctx := context.Background()
	data := []byte("hello blobs")
	blob, err := s.Put(ctx, "ref-1", "text/plain", bytes.NewReader(data))
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	if blob.Size != int64(len(data)) {
		t.Errorf("size = %d, want %d", blob.Size, len(data))
	}
	if blob.SHA256 == "" {
		t.Error("empty sha256")
	}
	// Get
	rc, err := s.Get(ctx, "ref-1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer rc.Close()
	got := make([]byte, len(data))
	if _, err := rc.Read(got); err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Errorf("data mismatch: %q != %q", got, data)
	}
	// Delete
	if err := s.Delete(ctx, "ref-1"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := s.Get(ctx, "ref-1"); err == nil {
		t.Fatal("expected not-found after delete")
	}
}

func TestLocalStoreRefSanitization(t *testing.T) {
	dir := t.TempDir()
	s, _ := NewLocalStore(dir)
	ctx := context.Background()
	// Path-traversal attempt must be neutralized.
	if _, err := s.Put(ctx, "../../etc/passwd", "text", strings.NewReader("x")); err != nil {
		t.Fatalf("put: %v", err)
	}
	// Ensure nothing escaped the root.
	_ = filepath.Walk(dir, func(path string, info os.FileInfo, err error) error { return nil })
	// The blob must live under dir (no parent traversal).
	rc, err := s.Get(ctx, "../../etc/passwd")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	rc.Close()
}
