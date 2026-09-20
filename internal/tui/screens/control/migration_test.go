package control

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/beardedparrott/orchicon/internal/tui/mutate"
)

// mutateResult aliases the mutation result message so the test can type
// assert the cmd's output without importing tea message plumbing.
type mutateResult = mutate.Result

// scanScreens reads every screen.go in the sibling screen packages.
func scanScreens(t interface{ Fatalf(string, ...any) }) string {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatalf("cannot locate the test file")
	}
	root := filepath.Join(filepath.Dir(file), "..")
	var b strings.Builder
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("read screens dir: %v", err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		p := filepath.Join(root, e.Name(), "screen.go")
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		b.Write(data)
	}
	return b.String()
}

func bytesContainAny(haystack, needle string) bool { return strings.Contains(haystack, needle) }
