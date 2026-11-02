package mcpclient

// mcp_legacy_protocol_test.go — the client must not treat a PROTOCOL MISMATCH as a broken server.
//
// REPORTED, after a whole session of misdiagnosis: "we are speaking a new MCP version and the aseprite is
// speaking an old one." The client defaults to 2026-07-28, which sends `server/discover` before
// `initialize`; an old server does not know that method, fails validation and EXITS. The connect then burns
// its whole timeout and reports a timeout against a dead child.
//
// These tests run against internal/mcpclient/legacy_fixture_test.go, a hand-rolled OLD server that does
// exactly that: dies on server/discover, fine on initialize.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// legacySpec is a stdio server pointing at the legacy fixture.
func legacySpec(t *testing.T, id string) (ServerSpec, string) {
	t.Helper()
	logPath := filepath.Join(t.TempDir(), "methods.log")
	spec := ServerSpec{
		ID:      id,
		Type:    TypeStdio,
		Command: legacyFixtureArgs(),
		Env: map[string]string{
			legacyFixtureEnv:    "1",
			legacyFixtureLogEnv: logPath,
			// The child re-execs the test binary; without this the fixture's TestMain would ALSO set the
			// bare fixture marker and the child could answer as the wrong server.
			"ORCHICON_MCP_FIXTURE": "",
		},
		Timeout: 10 * time.Second,
	}
	return spec, logPath
}

// methodsSeen returns the RPC methods the fixture received, in order.
func methodsSeen(t *testing.T, logPath string) []string {
	t.Helper()
	b, err := os.ReadFile(logPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("read fixture log: %v", err)
	}
	var out []string
	for _, line := range strings.Split(string(b), "\n") {
		if strings.TrimSpace(line) != "" {
			out = append(out, strings.TrimSpace(line))
		}
	}
	return out
}

// THE REPORTED BUG, end to end: an old-spec server yields its tools anyway.
//
// Before this fix the connect failed outright — and because one failing server used to abort the whole
// union, the operator lost every other server's tools too.
func TestALegacyServerStillYieldsItsTools(t *testing.T) {
	spec, logPath := legacySpec(t, "legacy")
	m := NewManager(quietMCPLogger())
	t.Cleanup(func() { _ = m.Close() })

	defs, err := m.Start(context.Background(), []ServerSpec{spec})
	if err != nil {
		t.Fatalf("Start failed for a legacy-spec server: %v\n  The client sends server/discover by default; "+
			"an old server exits on it, and the whole connection used to be lost. Methods the server saw: %v",
			err, methodsSeen(t, logPath))
	}
	if len(defs) == 0 {
		t.Fatal("the legacy server yielded no tools")
	}
	if got := defs[0].Name; !strings.Contains(got, "legacy_echo") {
		t.Fatalf("discovered tool %q, want the fixture's legacy_echo", got)
	}
	// The tool must be CALLABLE, not merely listed.
	if _, err := m.Execute(context.Background(), defs[0].Name, "{}"); err != nil {
		t.Fatalf("the legacy server's tool is not callable: %v", err)
	}

	// AND THE EVIDENCE THAT THE DEGRADE IS WHAT FIXED IT: the server saw the modern probe first (and died
	// on it), then a SECOND child ran the legacy handshake. Two children, one server definition.
	methods := methodsSeen(t, logPath)
	if !contains(methods, "server/discover") {
		t.Errorf("the fixture never received server/discover (%v) — the test is no longer exercising the "+
			"protocol mismatch it exists for", methods)
	}
	if !contains(methods, "initialize") {
		t.Errorf("the fixture never received initialize (%v) — the legacy retry did not run", methods)
	}
}

// Pinning the version skips the doomed probe entirely — one child, no wasted timeout.
func TestAPinnedLegacyServerNeverSeesTheModernProbe(t *testing.T) {
	spec, logPath := legacySpec(t, "pinned")
	spec.ProtocolVersion = protocolVersionLegacy
	m := NewManager(quietMCPLogger())
	t.Cleanup(func() { _ = m.Close() })

	defs, err := m.Start(context.Background(), []ServerSpec{spec})
	if err != nil {
		t.Fatalf("Start failed for a server PINNED to the legacy version: %v", err)
	}
	if len(defs) == 0 {
		t.Fatal("the pinned legacy server yielded no tools")
	}
	if methods := methodsSeen(t, logPath); contains(methods, "server/discover") {
		t.Fatalf("a PINNED legacy server still received server/discover (%v) — the pin is not being honoured, "+
			"so the operator's override buys nothing", methods)
	}
}

// "auto" means "let the client negotiate", which is the same as unset.
func TestAutoProtocolVersionIsTreatedAsUnset(t *testing.T) {
	if got := pinnedProtocolVersion(ServerSpec{ProtocolVersion: "auto"}); got != "" {
		t.Fatalf("pinnedProtocolVersion(\"auto\") = %q, want \"\" (negotiate)", got)
	}
	if got := pinnedProtocolVersion(ServerSpec{ProtocolVersion: " AUTO "}); got != "" {
		t.Fatalf("pinnedProtocolVersion(\" AUTO \") = %q, want \"\" — the value is operator-typed and must "+
			"tolerate spacing and case", got)
	}
	if got := pinnedProtocolVersion(ServerSpec{ProtocolVersion: protocolVersionLegacy}); got != protocolVersionLegacy {
		t.Fatalf("pinnedProtocolVersion(legacy) = %q, want %q", got, protocolVersionLegacy)
	}
}

// A server whose command does not exist must NOT be retried: a different protocol version cannot conjure a
// missing interpreter, and retrying would double the wait before the same answer.
func TestANeverStartedServerIsNotRetried(t *testing.T) {
	spec := deadSpec("missing")
	m := NewManager(quietMCPLogger())
	t.Cleanup(func() { _ = m.Close() })

	start := time.Now()
	if _, err := m.Start(context.Background(), []ServerSpec{spec}); err == nil {
		t.Fatal("Start succeeded for a server whose command does not exist")
	}
	elapsed := time.Since(start)

	// One attempt, not two. The bound is generous — this asserts "did not try twice", not a duration.
	if elapsed > 5*time.Second {
		t.Fatalf("a never-started server took %s — it was retried with the legacy version, which cannot help "+
			"when the command itself is wrong", elapsed)
	}
	msg := m.Failures()[0].Error()
	if strings.Contains(msg, "BOTH the modern and the legacy") {
		t.Fatalf("a never-started server was retried: %s", msg)
	}
	if !strings.Contains(msg, "never started") {
		t.Errorf("the diagnosis does not name the real cause: %s", msg)
	}
}

// When BOTH handshakes fail, the error says so — otherwise a reader concludes only one was tried.
func TestBothHandshakesFailingIsReportedAsBoth(t *testing.T) {
	sh := "/bin/sh"
	spec := ServerSpec{
		ID:      "hopeless",
		Type:    TypeStdio,
		Timeout: 2 * time.Second,
		// Starts, then exits without answering anything — so it is retried, and fails both times.
		Command: []string{sh, "-c", "echo 'nope' >&2; exit 7"},
	}
	m := NewManager(quietMCPLogger())
	t.Cleanup(func() { _ = m.Close() })

	if _, err := m.Start(context.Background(), []ServerSpec{spec}); err == nil {
		t.Fatal("Start succeeded for a server that answers nothing")
	}
	msg := m.Failures()[0].Error()
	if !strings.Contains(msg, "BOTH the modern and the legacy") {
		t.Fatalf("error does not report that BOTH handshakes were tried: %s", msg)
	}
	if !strings.Contains(msg, "nope") {
		t.Errorf("the child's stderr is missing from the combined error: %s", msg)
	}
}

// The SDK's gate is the constant this file reasons about. If a future bump moves it, the "already pinned"
// short-circuit would silently stop matching and the retry would repeat the failing attempt.
func TestLegacyVersionIsBelowTheSDKDiscoverGate(t *testing.T) {
	if !(protocolVersionLegacy < protocolVersionGate) {
		t.Fatalf("protocolVersionLegacy %q is NOT below the discover gate %q — pinning it would still send "+
			"server/discover and the whole fix becomes a no-op", protocolVersionLegacy, protocolVersionGate)
	}
}

func contains(hay []string, needle string) bool {
	for _, s := range hay {
		if s == needle {
			return true
		}
	}
	return false
}
