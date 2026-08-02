package runtime

import (
	"os"
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/guard"
)

func TestPrependGuardSmoke(t *testing.T) {
	dir, err := guard.MakeGuard("/tmp", "/tmp")
	if err != nil {
		t.Fatalf("MakeGuard: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	env := prependGuard(os.Environ(), dir)
	var got string
	for _, kv := range env {
		if strings.HasPrefix(kv, "PATH=") {
			got = strings.TrimPrefix(kv, "PATH=")
		}
	}
	if !strings.HasPrefix(got, dir) {
		t.Fatalf("expected PATH to start with %s, got %s", dir, got)
	}
	t.Logf("PATH starts with guard dir: %s...", got[:len(dir)])
}
