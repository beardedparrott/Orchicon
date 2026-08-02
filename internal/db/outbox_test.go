package db

import (
	"sync"
	"testing"
)

// TestNewIDConcurrent exercises NewID from many goroutines. The entropy
// source is ulid.Monotonic (NOT safe for concurrent use) — without the
// mutex in newULID this races on the shared crypto/rand bufio buffer and
// panics with "slice bounds out of range [:8192] with capacity 4096".
// Run with -race to detect the corruption deterministically.
func TestNewIDConcurrent(t *testing.T) {
	var wg sync.WaitGroup
	ids := make([]string, 0, 2000)
	var mu sync.Mutex
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 40; j++ {
				id := NewID()
				mu.Lock()
				ids = append(ids, id)
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if len(ids) != 2000 {
		t.Fatalf("expected 2000 ids, got %d", len(ids))
	}
	seen := map[string]bool{}
	for _, id := range ids {
		if len(id) != 26 {
			t.Fatalf("expected 26-char ULID, got %q (len %d)", id, len(id))
		}
		if seen[id] {
			t.Fatalf("duplicate id %q", id)
		}
		seen[id] = true
	}
}
