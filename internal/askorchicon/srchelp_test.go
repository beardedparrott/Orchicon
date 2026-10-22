package askorchicon

// srchelp_test.go — tiny source-reading helpers for the wiring assertions in this package.
//
// Some defects here are MISSING CALLS rather than wrong values (a retry that was never wired, a cancel
// that was skipped), and only the source states them. Reading chat.go is the honest way to pin those.

import (
	"os"
	"strings"
	"testing"
)

// readChatSource returns chat.go's text.
func readChatSource(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("chat.go")
	if err != nil {
		t.Fatalf("read chat.go: %v", err)
	}
	return string(b)
}

func indexOf(haystack, needle string) int { return strings.Index(haystack, needle) }
