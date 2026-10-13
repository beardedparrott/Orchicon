package aigateway

// retry_empty_test.go — AN EMPTY MODEL LIST IS A RACE, NOT AN ANSWER, SO IT IS RETRIED.
//
// MEASURED on the operator's machine, and the numbers are the whole justification:
//
//	 20 consecutive runs of `opencode models` -> 18,648 bytes each, ~65ms each
//	  1 run taken inside the managed service's location-boot window -> NOTHING
//
// opencode's own log shows why — the managed service EVICTS a location and lazily re-boots it
// ("location services evicted" … "location services booted", durationMs=23), and a listing that lands
// in that window sees an empty catalog. So an empty result is not a statement about the providers.
//
// TREATING IT AS DEFINITIVE is what made this a five-minute outage rather than a blip: the failure is
// cached (fetchOrCache's backoff), and a plane restart — which every rebuild performs — starts from a
// COLD cache with no prior list to serve. The first picker open after a rebuild was therefore the most
// likely to fail and the most likely to stick, which is exactly the report: "I rebuilt and it's still
// saying no models", having worked minutes earlier.
//
// The retry is bounded and is paid ONLY when the command returns empty; a normal discovery never
// enters it a second time.

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// emptyThenModelsStub prints nothing for the first `emptyCalls` invocations, then a model list.
// A counter file records every invocation so a test can assert HOW MANY attempts were made.
func emptyThenModelsStub(t *testing.T, emptyCalls int) (bin, callsPath string) {
	t.Helper()
	dir := t.TempDir()
	bin = filepath.Join(dir, "opencode")
	callsPath = filepath.Join(dir, "calls.txt")
	script := "#!/bin/sh\n" +
		"n=0\n" +
		"[ -f " + callsPath + " ] && n=$(wc -l < " + callsPath + ")\n" +
		"printf '%s\\n' \"$*\" >> " + callsPath + "\n" +
		"if [ \"$n\" -lt " + strconv.Itoa(emptyCalls) + " ]; then exit 0; fi\n" + // exit 0, print nothing
		"cat <<'OUT'\n" +
		"opencode/big-pickle\n" +
		"opencode/big-pickle-2\n" +
		"OUT\n" +
		"exit 0\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	return bin, callsPath
}

func callCount(t *testing.T, path string) int {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	return len(strings.Fields(strings.ReplaceAll(string(b), "\n", " "))) - 0
}

func TestATransientEmptyResultIsRetriedAndSucceeds(t *testing.T) {
	// THE REGRESSION PIN: the first attempt is empty (the service race), the second is not.
	bin, calls := emptyThenModelsStub(t, 1)
	d := NewModelDiscoverer(discardLog(), bin)
	d.SetModelsArgs([]string{"models"})

	models, err := d.fetchModels(context.Background())
	if err != nil {
		t.Fatalf("a single transient empty must NOT be reported as a failure: %v", err)
	}
	if len(models) != 2 {
		t.Errorf("got %d models, want the 2 the second attempt returned", len(models))
	}
	if got := strings.Count(string(mustRead(t, calls)), "models"); got < 2 {
		t.Errorf("expected the command to be run more than once (it was run %d time(s)) — without "+
			"the retry, one empty result inside the boot window becomes a reported failure", got)
	}
}

func TestAPersistentlyEmptyResultStillFails(t *testing.T) {
	// The retry must not become "keep trying until something appears": a genuinely empty list (no
	// provider authenticated) still has to fail, after a BOUNDED number of attempts.
	bin, calls := emptyThenModelsStub(t, 999)
	d := NewModelDiscoverer(discardLog(), bin)
	d.SetModelsArgs([]string{"models"})

	_, err := d.fetchModels(context.Background())
	if err == nil {
		t.Fatal("a persistently empty list must still be an error")
	}
	if !strings.Contains(err.Error(), "no models") {
		t.Errorf("the error must still describe the empty list, got: %v", err)
	}
	if n := strings.Count(mustRead(t, calls), "models"); n != emptyRetryAttempts {
		t.Errorf("expected exactly %d attempts, saw %d — the retry must be BOUNDED", emptyRetryAttempts, n)
	}
}

func TestAnUnparseableResultIsNotRetried(t *testing.T) {
	// The other failure mode — the command PRINTED something we cannot read — is deterministic. Retrying
	// it would only delay a real diagnosis, so it must be attempted exactly once.
	dir := t.TempDir()
	bin := filepath.Join(dir, "opencode")
	calls := filepath.Join(dir, "calls.txt")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> " + calls + "\n" +
		"echo 'this build emits a format we do not understand'\nexit 0\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatalf("write: %v", err)
	}

	d := NewModelDiscoverer(discardLog(), bin)
	d.SetModelsArgs([]string{"models"})

	if _, err := d.fetchModels(context.Background()); err == nil {
		t.Fatal("expected an error for an unparseable result")
	}
	if n := strings.Count(mustRead(t, calls), "models"); n != 1 {
		t.Errorf("an unparseable result was attempted %d times, want 1 — it is deterministic, so "+
			"retrying only delays the diagnosis", n)
	}
}

func TestASuccessfulListIsAttemptedOnce(t *testing.T) {
	// And the retry costs nothing in the normal case, which is the property that makes it acceptable.
	bin, calls := emptyThenModelsStub(t, 0)
	d := NewModelDiscoverer(discardLog(), bin)
	d.SetModelsArgs([]string{"models"})

	if _, err := d.fetchModels(context.Background()); err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if n := strings.Count(mustRead(t, calls), "models"); n != 1 {
		t.Errorf("a successful list took %d attempts, want 1 — the retry is paid only on an empty result", n)
	}
}

func mustRead(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(b)
}
