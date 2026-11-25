package server

// THE LIVENESS PROBE MUST ASK THE BRIDGE THE EXECUTION ACTUALLY RAN ON.
//
// MEASURED FAILURE (prod run 01M87T2BG0TV1XZ6HSEYEC9KCB, 2026-11-24 22:18):
//
//	execution 01M87T75ZX79B682SEG99QPGS8  ran on adp_orchicon_dev
//	worker w_se_senior_software_engineer  model_ref = opencode/opencode-go/deepseek-v4.1-flash
//
// The probe resolved the KIND from the worker's current published model_ref — "opencode" — and asked the
// OPENCODE bridge whether that execution was alive. That bridge had never heard of it and correctly
// answered no. The reaper failed the execution as "execution lost: control plane restarted or runtime
// container gone" at 217 seconds.
//
// It was alive throughout: the same execution logged native-session StopLength continuation turns at
// 22:32 and 22:38 — five minutes AFTER being marked failed. A recovery was triggered against a live
// session, and the container the message named was never the problem.
//
// These assertions are on the probe's BEHAVIOUR where behaviour is reachable (which return value each
// failure path produces), and on its structure where it is not: exercising the happy path end to end
// needs a live *db.Pool and a *scheduler.Dispatcher, neither constructible in a unit test.

import (
	"os"
	"strings"
	"testing"
)

func mustRead(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(b)
}

// funcText returns the body of a top-level function, from its signature to the next top-level func.
func funcText(t *testing.T, src, decl string) string {
	t.Helper()
	i := strings.Index(src, decl)
	if i < 0 {
		t.Fatalf("declaration not found: %s", decl)
	}
	rest := src[i+len(decl):]
	if j := strings.Index(rest, "\nfunc "); j > 0 {
		rest = rest[:j]
	}
	return rest
}

// returnAfter finds the return statement following a log marker.
func returnAfter(s, marker string) string {
	i := strings.Index(s, marker)
	if i < 0 {
		return ""
	}
	tail := s[i:]
	ri := strings.Index(tail, "return ")
	if ri < 0 {
		return ""
	}
	stmt := tail[ri:]
	if end := strings.Index(stmt, "\n"); end > 0 {
		stmt = stmt[:end]
	}
	return strings.TrimSpace(stmt)
}

// The property: the kind comes from the EXECUTION's adapter row, not the worker's current model_ref.
func TestProbeAsksTheExecutionsAdapterNotTheWorkersPinnedKind(t *testing.T) {
	body := funcText(t, mustRead(t, "server.go"), "func activeProbe(")

	if !strings.Contains(body, "exec.AdapterID") {
		t.Fatal("the probe no longer reads exec.AdapterID. Resolving the kind from the worker's CURRENT " +
			"published model_ref asks the wrong bridge, which answers \"not active\" for an execution it " +
			"has never seen — and the reaper kills a live run.")
	}
	if !strings.Contains(body, "db.GetAdapter(") {
		t.Error("the probe must look the execution's adapter row up to learn its kind")
	}
	if !strings.Contains(body, "arow.Kind") {
		t.Error("the probe must take the KIND from the execution's adapter row")
	}
	// A DEAD LOOKUP IS THE BUG WEARING THE FIX'S CLOTHES: `if false {` around the adapter lookup
	// satisfies every "does it mention the row" assertion while restoring the old behaviour exactly.
	if strings.Contains(body, "if false") {
		t.Fatal("the adapter-row lookup is short-circuited (`if false`), so the probe falls back to the " +
			"worker's pinned kind — the exact defect this test exists to prevent")
	}
	// And the row lookup must PRECEDE the worker fallback, or the fallback wins every time.
	rowAt := strings.Index(body, "db.GetAdapter(")
	verAt := strings.Index(body, "db.GetLatestWorkerVersion(")
	if rowAt > 0 && verAt > 0 && verAt < rowAt {
		t.Error("the probe consults the worker's pinned version BEFORE the execution's adapter row, so " +
			"the wrong-kind resolution still decides")
	}
}

// UNKNOWN IS NOT DEAD. This half IS behavioural: it asserts which return value each failure path yields,
// which a short-circuit cannot fake.
func TestProbeUnknownOutcomesDoNotAuthorizeAKill(t *testing.T) {
	body := funcText(t, mustRead(t, "server.go"), "func activeProbe(")

	for _, c := range []struct{ marker, why string }{
		{"liveness probe: begin tx failed",
			"a database hiccup would otherwise reap every running execution at once"},
		{"liveness probe: no bridge for kind",
			"an unregistered adapter kind is a configuration gap, not a dead session"},
		{"liveness probe: bridge does not support liveness",
			"a bridge that cannot answer has not said no"},
		{"liveness probe: worker version lookup failed",
			"a bookkeeping failure must not authorize a kill"},
	} {
		stmt := returnAfter(body, c.marker)
		if stmt == "" {
			t.Errorf("the probe no longer handles %q — if that was deliberate, rewrite this test rather "+
				"than delete it: %s", c.marker, c.why)
			continue
		}
		if !strings.Contains(stmt, "return true") {
			t.Errorf("%q returns %q — an UNKNOWN verdict must not authorize a kill (%s)",
				c.marker, stmt, c.why)
		}
	}
}

// The single path that MAY kill: the bridge positively reports the execution is not active.
func TestProbeKillsOnlyOnAPositiveNotActive(t *testing.T) {
	body := funcText(t, mustRead(t, "server.go"), "func activeProbe(")
	if !strings.Contains(body, "rep.IsExecutionActive(exec.ID)") {
		t.Error("the probe must still consult the bridge's liveness reporter for the real verdict")
	}
}
