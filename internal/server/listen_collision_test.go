package server

// Tests for the bind-set collision guard (listen.go). The host plane used to
// boot with a WILDCARD primary (`ORCHICON_HTTP_ADDR=:8080`) plus the
// docker-bridge extra bind: the wildcard already owns <bridge ip>:8080 on every
// interface, so serveExtra's net.Listen could never succeed and the plane
// retried it every 30s forever ("bridge listener bind failed — runtime-
// container workers cannot reach the plane … address already in use").

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/beardedparrott/orchicon/internal/config"
)

func TestBindSetCollides(t *testing.T) {
	cases := []struct {
		primary, extra string
		want           bool
	}{
		{":8080", "172.17.0.1:8080", true},            // the host plane's broken shape
		{"0.0.0.0:8080", "172.17.0.1:8080", true},     // explicit v4 wildcard
		{"[::]:8091", "172.17.0.1:8091", true},        // explicit v6 wildcard
		{"127.0.0.1:8080", "172.17.0.1:8080", false},  // the fixed shape
		{"172.17.0.1:8080", "172.17.0.1:8080", false}, // same port, concrete hosts
		{"0.0.0.0:8080", "172.17.0.1:8091", false},    // wildcard, different port
		{"127.0.0.1:8080", "", false},                 // no extra bind at all
		{"", "172.17.0.1:8080", false},                // malformed primary
		{"8080", "172.17.0.1:8080", false},            // not host:port
	}
	for _, c := range cases {
		if got := bindSetCollides(c.primary, c.extra); got != c.want {
			t.Errorf("bindSetCollides(%q, %q) = %v, want %v", c.primary, c.extra, got, c.want)
		}
	}
}

// A wildcard primary is reported LOUD and ONCE, naming the env var that fixes
// it, and the doomed bridge goroutine is never started — neither its success
// nor its failure line can appear, and there is no 30s retry loop.
func TestWildcardPrimarySkipsDoomedBridgeBind(t *testing.T) {
	prev := extraBindRetryInterval
	extraBindRetryInterval = 10 * time.Millisecond
	defer func() { extraBindRetryInterval = prev }()

	var buf syncBuf
	// Both port-0 so the wildcard primary and the extra bind name the same
	// port without needing a privileged one: that IS the collision.
	s := newListenerTestServer(config.Config{HTTPAddr: ":0", ExtraBind: "127.0.0.1:0"}, &buf)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	primary, err := s.startListeners(ctx)
	if err != nil {
		t.Fatalf("startListeners: %v", err)
	}
	defer func() { _ = primary.Close() }()

	if !waitForLog(t, &buf, "bridge listener skipped", 2*time.Second) {
		t.Fatalf("a wildcard primary plus an extra bind was not reported; log:\n%s", buf.String())
	}
	if logs := buf.String(); !strings.Contains(logs, "ORCHICON_HTTP_ADDR") {
		t.Errorf("the collision report must name ORCHICON_HTTP_ADDR; log:\n%s", logs)
	}

	time.Sleep(100 * time.Millisecond)
	logs := buf.String()
	for _, forbidden := range []string{"bridge listener serving", "bridge listener bind failed"} {
		if strings.Contains(logs, forbidden) {
			t.Errorf("the doomed bridge listener was still started (%q present); log:\n%s", forbidden, logs)
		}
	}
}
