package mcpclient

// config_test.go — AC 3 and AC 1's observation seam, at the pure level.
//
// The provenance formatter and the scope-naming failure annotator are the two
// pieces BOTH adapters consume. Testing them here is what makes the per-adapter
// assertions in internal/claude and internal/orchicon read the SAME behaviour
// rather than two copies that happen to agree today.

import (
	"errors"
	"strings"
	"testing"
)

// ProvenanceString is order-stable, names the supplying scope per server, and
// NEVER touches Env/Headers (which the caller replaces with resolved plaintext
// before the connection is established).
func TestProvenanceString(t *testing.T) {
	got := ProvenanceString([]ScopedServer{
		{
			Spec:   ServerSpec{ID: "proj-srv", Env: map[string]string{"TOKEN": "super-secret"}},
			From:   ScopeProject,
			FromID: "project:p1",
		},
		{
			Spec:   ServerSpec{ID: "gh", Headers: map[string]string{"Authorization": "Bearer super-secret"}},
			From:   ScopeWorker,
			FromID: "inline:w1@3",
		},
	})
	want := "proj-srv=project:p1,gh=inline:w1@3"
	if got != want {
		t.Fatalf("ProvenanceString = %q, want %q", got, want)
	}
	if strings.Contains(got, "super-secret") {
		t.Fatalf("provenance leaked a credential: %q", got)
	}

	// Empty resolution renders empty (an absent MCP surface logs "" not "<nil>").
	if s := ProvenanceString(nil); s != "" {
		t.Errorf("ProvenanceString(nil) = %q, want empty", s)
	}
}

// A server whose FromID is unset falls back to its Kind, so a stub or a
// partial resolution still produces usable provenance rather than "srv=".
func TestProvenanceStringFallsBackToKind(t *testing.T) {
	got := ProvenanceString([]ScopedServer{{Spec: ServerSpec{ID: "s1"}, From: ScopeConversation}})
	if got != "s1=conversation" {
		t.Fatalf("ProvenanceString = %q, want s1=conversation", got)
	}
}

// AC 3: an unreachable server must name BOTH the server and the scope it came
// from. The manager quotes the id as `mcp server %q: ...` and stops at the
// first failure over the SAME ordered list, so the match is deterministic.
func TestDescribeFailedServerNamesTheScope(t *testing.T) {
	servers := []ScopedServer{
		{Spec: ServerSpec{ID: "proj-srv"}, From: ScopeProject, FromID: "project:p1"},
		{Spec: ServerSpec{ID: "gh"}, From: ScopeWorker, FromID: "inline:w1@3"},
	}
	err := errors.New(`mcp server "gh": connect failed (stdio transport): exec: "nope": executable file not found`)
	got := DescribeFailedServer(err, servers)
	if got == nil {
		t.Fatal("DescribeFailedServer dropped the error")
	}
	msg := got.Error()
	if !strings.Contains(msg, `"gh"`) {
		t.Errorf("the failure does not name the server: %v", msg)
	}
	if !strings.Contains(msg, "inline:w1@3") {
		t.Errorf("the failure does not name the scope: %v", msg)
	}
	if !strings.Contains(msg, "connect failed") {
		t.Errorf("the failure lost its cause: %v", msg)
	}
	// The original error stays wrapped (errors.Is/As still reach it).
	if !errors.Is(got, err) {
		t.Errorf("the original error is no longer reachable: %v", msg)
	}
}

// A manager-level error that names no server passes through unchanged: there is
// no scope to attribute it to, and inventing one would be a lie in the log.
func TestDescribeFailedServerPassesThroughWhenNoServerMatches(t *testing.T) {
	err := errors.New("mcp client: manager closed")
	if got := DescribeFailedServer(err, []ScopedServer{{Spec: ServerSpec{ID: "gh"}}}); got != err {
		t.Fatalf("an unmatched error was rewritten: %v", got)
	}
	if got := DescribeFailedServer(nil, []ScopedServer{{Spec: ServerSpec{ID: "gh"}}}); got != nil {
		t.Fatalf("nil error became %v", got)
	}
}
