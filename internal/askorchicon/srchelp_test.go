package askorchicon

// srchelp_test.go — tiny source-reading helpers for the wiring assertions in this package.

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
