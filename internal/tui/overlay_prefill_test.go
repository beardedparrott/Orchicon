package tui

import (
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/tui/config"
)

// Reproduces the operator's Phase 3 connect-overlay trap: a password-mode
// profile with a long (expired) access token opened the overlay with the
// credential field PRE-FILLED — a ~300-char echo row the operator could
// not reason about or replace. The overlay must start the credential
// EMPTY (URL/username may stay pre-filled).
func TestConnectOverlayCredentialStartsEmptyOnReauth(t *testing.T) {
	stale := strings.Repeat("eyJhbGciOiJIUzI1NiJ9.x", 20) // ~460 chars
	m := NewApp(nil, &config.Profile{
		URL:        "http://localhost:8080",
		AuthMethod: config.AuthPassword,
		Token:      stale,
		Username:   "orchicon",
	}, "")
	m.width, m.height = 120, 40
	m.openConnectOverlay()
	if !m.palette.connectOpen {
		t.Fatal("overlay did not open")
	}
	if m.palette.connectForm == nil {
		t.Fatal("connect form is nil")
	}
	v := m.connectOverlayView()
	if strings.Contains(v, "eyJhbGciOiJIUzI1NiJ9") {
		t.Fatal("stale token leaked into the overlay render")
	}
	// The credential echo row must be short (an empty field echoes at most
	// a couple of chars), never a full-width dotted line.
	for _, line := range strings.Split(v, "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "Password") {
			continue
		}
		if n := strings.Count(line, "•"); n > 5 {
			t.Fatalf("credential field pre-filled: %d echo chars (want <=5)", n)
		}
	}
}
