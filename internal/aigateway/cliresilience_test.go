package aigateway

// cliresilience_test.go — ONE retry policy for every CLI call, and the line between transient and
// deterministic drawn where it belongs.
//
// THE OPERATOR: "resilience across the board, not just on one provider or one model type." The gateway had it
// on the model catalog and NOT on the MCP config — same CLI, same managed service, same eviction window — so
// a blip the model list absorbed as a retry surfaced on the MCP list as a failure. These tests pin the shared
// policy and, just as importantly, pin the refusals: a deterministic failure must be reported at once, because
// retrying it turns a precise diagnosis into a slow, vague one.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRetryableCLIErrorDrawsTheLine(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		// TRANSIENT: the managed service mid-eviction.
		{"empty catalog (the documented race)", errNoModels, true},
		{"the CLI hung", fmt.Errorf("opencode models: %w", context.DeadlineExceeded), true},
		{"an unexplained non-zero exit", fmt.Errorf("opencode models: %w", errors.New("exit status 1")), true},

		// DETERMINISTIC: it told us what was wrong, so another attempt collects the same words.
		{"unparseable output", errors.New("opencode printed 42 bytes but no models could be read from them — " +
			"this build's output format is not one this version understands"), false},
		{"an unknown flag (the CLI explained itself)", &cliError{
			invocation: "opencode models --verbose", err: errors.New("exit status 1"),
			detail: "unknown flag: --verbose"}, false},
		{"a bad config", errors.New("parse opencode config: unexpected end of JSON input"), false},
		{"the binary is missing", errors.New("exec: \"opencode\": executable file not found in $PATH"), false},

		// THE CALLER SAID STOP. Retrying would ignore an instruction.
		{"the caller cancelled", fmt.Errorf("models: %w", context.Canceled), false},
		{"no error", nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := retryableCLIError(c.err); got != c.want {
				t.Errorf("retryableCLIError(%v) = %v, want %v", c.err, got, c.want)
			}
		})
	}
}

// A DETERMINISTIC FAILURE IS ATTEMPTED EXACTLY ONCE — the property the first classifier got wrong by
// reasoning from the error TYPE (a parse error is not an *exec.ExitError, so it fell through to "retry").
func TestDeterministicFailureIsNotRetried(t *testing.T) {
	attempts := 0
	_, err := withCLIRetries(context.Background(), discardLog(), "test", false, func() (int, error) {
		attempts++
		return 0, errors.New("opencode printed 9 bytes but no models could be read from them")
	})
	if err == nil {
		t.Fatal("want an error")
	}
	if attempts != 1 {
		t.Errorf("a deterministic failure was attempted %d times, want 1 — retrying it only delays the "+
			"diagnosis", attempts)
	}
}

// A TRANSIENT FAILURE IS RETRIED, AND SUCCEEDS WHEN THE CONDITION CLEARS.
func TestTransientFailureIsRetriedUntilItClears(t *testing.T) {
	attempts := 0
	n, err := withCLIRetries(context.Background(), discardLog(), "test", false, func() (int, error) {
		attempts++
		if attempts < 3 {
			return 0, fmt.Errorf("opencode: %w", errors.New("exit status 1"))
		}
		return 7, nil
	})
	if err != nil {
		t.Fatalf("the transient failure never cleared: %v", err)
	}
	if attempts != 3 {
		t.Errorf("attempts = %d, want 3", attempts)
	}
	if n != 7 {
		t.Errorf("n = %d, want 7", n)
	}
}

// AN EMPTY RESULT IS TRANSIENT ONLY WHERE IT IS KNOWN TO BE A RACE. The model catalog reports an empty list
// when the service is re-booting; the MCP config's empty result is a legitimate answer (no servers), so it
// must NOT be retried — the caller states which, rather than the loop guessing.
func TestEmptyIsTransientOnlyWhenTheCallerSaysSo(t *testing.T) {
	t.Run("empty is a race", func(t *testing.T) {
		attempts := 0
		_, err := withCLIRetries(context.Background(), discardLog(), "models", true, func() (int, error) {
			attempts++
			return 0, nil // empty, no error
		})
		if err == nil {
			t.Fatal("an empty catalog that never fills must end in an error")
		}
		if attempts != cliRetryAttempts {
			t.Errorf("attempts = %d, want %d", attempts, cliRetryAttempts)
		}
	})
	t.Run("empty is an answer", func(t *testing.T) {
		attempts := 0
		_, err := withCLIRetries(context.Background(), discardLog(), "mcp", false, func() (int, error) {
			attempts++
			return 0, nil
		})
		if err != nil {
			t.Fatalf("an empty-but-valid result must not error: %v", err)
		}
		if attempts != 1 {
			t.Errorf("attempts = %d, want 1 — an empty config is a legitimate answer", attempts)
		}
	})
}

// THE RETRY IS BOUNDED, so a wedged CLI produces an error rather than a discoverer that never returns.
func TestCLIRetriesAreBounded(t *testing.T) {
	attempts := 0
	_, err := withCLIRetries(context.Background(), discardLog(), "test", false, func() (int, error) {
		attempts++
		return 0, errors.New("opencode: exit status 1")
	})
	if err == nil {
		t.Fatal("want an error after the budget is spent")
	}
	if attempts != cliRetryAttempts {
		t.Errorf("attempts = %d, want the bound %d", attempts, cliRetryAttempts)
	}
}

// THE MCP DISCOVERER NOW RETRIES — the asymmetry this file exists to remove, pinned through the REAL path
// (a stub CLI that fails transiently, then succeeds) rather than by calling the loop directly.
func TestMCPDiscoveryRetriesATransientFailure(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "opencode")
	calls := filepath.Join(dir, "calls.txt")
	// Fails twice with an exit that SAYS NOTHING, then returns a valid config.
	//
	// "Says nothing" is the transient shape and it is deliberate: a CLI that exits non-zero WITH a message
	// has explained itself, and an explained failure is deterministic (retrying collects the same words).
	// Writing a message here made this stub deterministic and the test correctly failed — the classifier was
	// right and the fixture was wrong, which is worth keeping in the test as a note.
	script := "#!/bin/sh\n" +
		"printf 'call\\n' >> " + calls + "\n" +
		"n=$(wc -l < " + calls + ")\n" +
		"if [ \"$n\" -lt 3 ]; then exit 1; fi\n" +
		"echo '{\"mcp\":{\"my-server\":{\"command\":\"run-me\"}}}'\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}

	// The binary is the stub by ABSOLUTE PATH, and the discoverer is given it directly — no PATH lookup, so a
	// real opencode on the machine cannot answer instead (it did, on the first run of this test).
	d := NewMCPDiscoverer(discardLog(), bin)
	servers, err := d.ListMCPs(context.Background())
	if err != nil {
		t.Fatalf("MCP discovery failed despite a transient failure that clears: %v", err)
	}
	// The result is the stub's configured server MERGED WITH the well-known catalog (mergeMCPs always adds
	// those, so the picker is never empty), so the assertion is on the stub's server being PRESENT rather than
	// on the list being exactly one — which is what the first version of this test got wrong.
	var found bool
	for _, sv := range servers {
		if sv.Id == "my-server" && sv.Command == "run-me" {
			found = true
		}
	}
	if !found {
		t.Fatalf("the stub's configured server is missing from the result: %+v", servers)
	}
	if n := strings.Count(mustRead(t, calls), "call"); n < 3 {
		t.Errorf("the stub was called %d times, want at least 3 — the MCP path must RETRY a transient "+
			"failure, which is the whole point of sharing the policy", n)
	}
}

// AND A DETERMINISTIC MCP FAILURE IS STILL REPORTED AT ONCE.
func TestMCPDiscoveryDoesNotRetryADeterministicFailure(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "opencode")
	calls := filepath.Join(dir, "calls.txt")
	script := "#!/bin/sh\n" +
		"printf 'call\\n' >> " + calls + "\n" +
		"echo 'not json at all'\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}

	d := NewMCPDiscoverer(discardLog(), bin)
	if _, err := d.ListMCPs(context.Background()); err == nil {
		t.Fatal("want an error for unparseable config output")
	}
	if n := strings.Count(mustRead(t, calls), "call"); n != 1 {
		t.Errorf("the stub was called %d times, want 1 — an unparseable result is deterministic", n)
	}
}

var _ = time.Second
