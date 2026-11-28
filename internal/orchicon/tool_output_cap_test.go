package orchicon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The tool-output cap is the single largest lever on per-turn context: every byte kept here is re-sent
// on every subsequent turn of the session. Measured on run 01M8FE5CWEZ9WP7QXQREDJDM9D the context sat at
// ~78k-92k tokens per turn and grew ~600 tokens every turn, at ~1.25s/turn, for over an hour.

func TestMaxToolOutputBytesIsTunable(t *testing.T) {
	// Default.
	t.Setenv("ORCHICON_MAX_TOOL_OUTPUT_BYTES", "")
	if got := maxToolOutputBytes(); got != maxToolOutputBytesDefault {
		t.Fatalf("maxToolOutputBytes() = %d, want the default %d", got, maxToolOutputBytesDefault)
	}
	// Operator-tunable through the SAME var the opencode adapter honours — the asymmetry (native hardcoded,
	// opencode settable) was the defect.
	t.Setenv("ORCHICON_MAX_TOOL_OUTPUT_BYTES", "4096")
	if got := maxToolOutputBytes(); got != 4096 {
		t.Fatalf("maxToolOutputBytes() = %d, want 4096 from the env var", got)
	}
	t.Setenv("ORCHICON_MAX_TOOL_OUTPUT_BYTES", "not-a-number")
	if got := maxToolOutputBytes(); got != maxToolOutputBytesDefault {
		t.Fatalf("an invalid value must fall back to the default, got %d", got)
	}
}

func TestCapToolOutputShortIsUntouched(t *testing.T) {
	got := capToolOutput("small output", t.TempDir(), "exec1", "call1")
	if got != "small output" {
		t.Fatalf("a result under the cap must pass through unchanged, got %q", got)
	}
}

// TestCapToolOutputKeepsHeadAndTail is the regression for the discard: the tail carries stack traces and
// trailing test failures, and losing it made workers RE-RUN the command — paying for the whole output
// again on top of a context that now held both attempts.
func TestCapToolOutputKeepsHeadAndTail(t *testing.T) {
	t.Setenv("ORCHICON_MAX_TOOL_OUTPUT_BYTES", "4096")
	dir := t.TempDir()
	head := "HEAD-MARKER: build started\n"
	tail := "\nTAIL-MARKER: FAIL github.com/x/y — test TestFoo failed"
	body := strings.Repeat("filler line of build output\n", 5000)
	full := head + body + tail

	got := capToolOutput(full, dir, "exec1", "call1")
	if len(got) > 4096 {
		t.Fatalf("capped output is %d bytes, over the 4096 cap", len(got))
	}
	if !strings.HasPrefix(got, "HEAD-MARKER") {
		t.Errorf("the head must survive — it is the decision/summary part:\n%q", got[:80])
	}
	if !strings.Contains(got, "TAIL-MARKER: FAIL") {
		t.Errorf("the tail must survive — a worker that cannot see the failure re-runs the command:\n%q", got)
	}
	if !strings.Contains(got, "truncated by Orchicon") {
		t.Errorf("the cap must be visible to the model, not silent:\n%q", got)
	}
}

// TestCapToolOutputOffloadsFullPayload asserts the payload is reachable on disk and referenced in the
// transcript, instead of being discarded.
func TestCapToolOutputOffloadsFullPayload(t *testing.T) {
	t.Setenv("ORCHICON_MAX_TOOL_OUTPUT_BYTES", "4096")
	dir := t.TempDir()
	full := strings.Repeat("a line of output that must not be lost\n", 5000)

	got := capToolOutput(full, dir, "exec1", "call1")
	if !strings.Contains(got, "offloaded to") {
		t.Fatalf("an oversized payload must be offloaded with a reference in the transcript:\n%q", got)
	}
	p := filepath.Join(dir, ".orchicon", "outputs", "exec1", "call1.out")
	onDisk, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("the offloaded payload must be readable at %s: %v", p, err)
	}
	if string(onDisk) != full {
		t.Fatalf("the offloaded payload must be the FULL output (%d bytes on disk, want %d)", len(onDisk), len(full))
	}
	// The reference the model sees must name the path RELATIVE to the project dir, because the worker's
	// cwd IS that directory — an absolute path would be noise it cannot use as a relative read target.
	rel := filepath.Join(".orchicon", "outputs", "exec1", "call1.out")
	if !strings.Contains(got, rel) {
		t.Errorf("the transcript reference must name the offloaded path %q:\n%q", rel, got)
	}
}

// TestCapToolOutputWithoutProjectDirStillCaps pins that a session with no project dir cannot offload, and
// must still cap (losing the tail is acceptable; blowing the context is not).
func TestCapToolOutputWithoutProjectDirStillCaps(t *testing.T) {
	t.Setenv("ORCHICON_MAX_TOOL_OUTPUT_BYTES", "4096")
	full := strings.Repeat("x", 20000)
	got := capToolOutput(full, "", "exec1", "call1")
	if len(got) > 4096 {
		t.Fatalf("capped output is %d bytes, over the cap", len(got))
	}
	if strings.Contains(got, "offloaded to") {
		t.Errorf("with no project dir there is nowhere to offload; the reference must not claim otherwise:\n%q", got)
	}
}

// TestCapToolOutputSanitizesCallIDPath pins that a provider-supplied call id cannot escape the offload dir.
func TestCapToolOutputSanitizesCallIDPath(t *testing.T) {
	t.Setenv("ORCHICON_MAX_TOOL_OUTPUT_BYTES", "4096")
	dir := t.TempDir()
	full := strings.Repeat("y", 20000)
	_ = capToolOutput(full, dir, "exec1", "../../etc/passwd")

	entries, err := os.ReadDir(filepath.Join(dir, ".orchicon", "outputs", "exec1"))
	if err != nil {
		t.Fatalf("offload dir: %v", err)
	}
	for _, e := range entries {
		if strings.ContainsAny(e.Name(), "/\\") {
			t.Errorf("offload filename %q still carries a path separator", e.Name())
		}
	}
	if len(entries) != 1 {
		t.Fatalf("expected exactly one offloaded file, got %d", len(entries))
	}
	// Nothing may be written OUTSIDE the offload dir.
	if _, err := os.Stat(filepath.Join(dir, "etc", "passwd.out")); err == nil {
		t.Fatalf("a traversal call id escaped the offload directory")
	}
}
