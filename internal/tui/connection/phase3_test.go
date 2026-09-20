package connection

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/tui/client"
	"github.com/beardedparrott/orchicon/internal/tui/config"
)

// TestInputsHaveExplicitWidth pins finding 2's field-width fix: a
// zero-width textinput never bounds its frame (the masked credential echo
// painted a full-width dotted line).
func TestInputsHaveExplicitWidth(t *testing.T) {
	m := New(&config.Profile{URL: "https://x.example.com"}, ProbeFuncs{})
	for i := range m.inputs {
		if m.inputs[i].Width != 40 {
			t.Fatalf("input %d width = %d, want 40", i, m.inputs[i].Width)
		}
	}
}

// TestLoginErrorWrappedOnce pins the dedupe of the doubled error prefix:
// client.Login already says "login failed:", so the probe must not wrap it
// a second time ("login failed: login failed: HTTP 401").
func TestLoginErrorWrappedOnce(t *testing.T) {
	m := New(&config.Profile{URL: "https://x.example.com", AuthMethod: config.AuthPassword, Username: "u"}, ProbeFuncs{
		Versionz: func(ctx context.Context, baseURL string, insecure bool) (*client.VersionzResponse, error) {
			return &client.VersionzResponse{Version: "v1.0.0"}, nil
		},
		LocalLogin: func(ctx context.Context, baseURL, username, password string, insecure bool) (*client.LoginResponse, error) {
			return nil, errors.New("login failed: HTTP 401")
		},
	})
	m.inputs[fieldCredential].SetValue("pw")
	_, err := m.probe(context.Background())
	if err == nil {
		t.Fatal("probe must fail")
	}
	if n := strings.Count(err.Error(), "login failed"); n != 1 {
		t.Fatalf("error = %q, want exactly one \"login failed\" prefix", err.Error())
	}
}
