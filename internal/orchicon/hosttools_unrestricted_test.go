package orchicon

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// hosttools_unrestricted_test.go pins BOTH halves of the boundary this change
// moves: the interactive suite (NewHostToolsUnrestricted) reaches the real
// filesystem, and the worker constructors (NewHostTools / NewContainerHostTools)
// remain confined and cannot inherit the widening. They live together so a
// future edit that lets the two converge fails here.

// TestUnrestrictedToolsReadOutsideWorktree: the interactive suite reads an
// absolute path outside its anchoring directory, through read, batch_read,
// list and glob — no escape error anywhere.
func TestUnrestrictedToolsReadOutsideWorktree(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(secret, []byte("real filesystem reach"), 0o644); err != nil {
		t.Fatal(err)
	}
	h := NewHostToolsUnrestricted(dir)
	ctx := context.Background()

	out, err := h.Execute(ctx, "read", `{"path":`+hostJSONString(t, secret)+`}`)
	if err != nil {
		t.Fatalf("read absolute outside the anchor was refused (interactive boundary too narrow): %v", err)
	}
	if !strings.Contains(out, "real filesystem reach") {
		t.Fatalf("read returned %q, want the outside file's content", out)
	}

	out, err = h.Execute(ctx, "batch_read", `{"paths":[`+hostJSONString(t, secret)+`,`+hostJSONString(t, outside)+`]}`)
	if err != nil || !strings.Contains(out, "secret.txt") {
		t.Fatalf("batch_read across the anchor boundary: err=%v out=%q", err, out)
	}

	out, err = h.Execute(ctx, "list", `{"paths":[`+hostJSONString(t, outside)+`]}`)
	if err != nil || !strings.Contains(out, "secret.txt") {
		t.Fatalf("list absolute outside the anchor: err=%v out=%q", err, out)
	}

	out, err = h.Execute(ctx, "glob", `{"pattern":"**/*.txt","path":`+hostJSONString(t, outside)+`}`)
	if err != nil || !strings.Contains(out, "secret.txt") {
		t.Fatalf("glob absolute outside the anchor: err=%v out=%q", err, out)
	}
}

// TestUnrestrictedRelativeStillAnchorsWorktree: a relative path is unchanged —
// it resolves against the anchoring directory (acceptance criterion 3).
func TestUnrestrictedRelativeStillAnchorsWorktree(t *testing.T) {
	dir := t.TempDir()
	h := NewHostToolsUnrestricted(dir)
	if _, err := h.Execute(context.Background(), "write", `{"filePath":"a.txt","content":"anchored"}`); err != nil {
		t.Fatalf("relative write: %v", err)
	}
	if data, err := os.ReadFile(filepath.Join(dir, "a.txt")); err != nil || string(data) != "anchored" {
		t.Fatalf("relative path did not anchor at the worktree: data=%q err=%v", data, err)
	}
}

// TestUnrestrictedBashCwdAndCdAway: bash's default cwd is the anchoring
// directory, and it can operate elsewhere after a `cd` (acceptance criterion 2).
func TestUnrestrictedBashCwdAndCdAway(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	h := NewHostToolsUnrestricted(dir)
	ctx := context.Background()

	out, err := h.Execute(ctx, "bash", `{"command":"pwd"}`)
	if err != nil {
		t.Fatalf("bash pwd: %v", err)
	}
	if !strings.Contains(out, dir) {
		t.Fatalf("bash default cwd = %q, want the anchor %q", out, dir)
	}

	out, err = h.Execute(ctx, "bash", `{"command":"cd `+outside+` && pwd"}`)
	if err != nil {
		t.Fatalf("bash cd elsewhere: %v", err)
	}
	if !strings.Contains(out, outside) {
		t.Fatalf("bash cd elsewhere printed %q, want %q", out, outside)
	}
}

// TestWorkerConstructorCannotInheritUnrestricted is the LEAK GATE: the widening
// is constructor-only, so the worker constructors must build a confined base —
// asserted structurally AND behaviourally.
func TestWorkerConstructorCannotInheritUnrestricted(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	abs := filepath.Join(outside, "secret.txt")

	h := NewHostTools(dir, dir)
	if h.base.AllowAnyPath {
		t.Fatal("NewHostTools inherited AllowAnyPath — the worker suite is no longer confined")
	}
	ch := NewContainerHostTools(dir, dir, func(_ context.Context, _ string, _ []string, _ string) (string, string, int, error) {
		return "", "", 0, nil
	})
	if ch.base.AllowAnyPath {
		t.Fatal("NewContainerHostTools inherited AllowAnyPath — the worker suite is no longer confined")
	}

	// Behavioural: the confined worker suite still refuses the absolute path,
	// through the composite resolver AND the delegated list resolver.
	ctx := context.Background()
	if _, err := h.Execute(ctx, "read", `{"path":`+hostJSONString(t, abs)+`}`); err == nil ||
		!strings.Contains(err.Error(), "outside the allowed roots") {
		t.Fatalf("worker read outside the roots: err=%v, want an allowed-roots refusal", err)
	}
	if _, err := h.Execute(ctx, "list", `{"paths":[`+hostJSONString(t, outside)+`]}`); err == nil ||
		!strings.Contains(err.Error(), "outside the allowed roots") {
		t.Fatalf("worker list outside the roots: err=%v, want an allowed-roots refusal", err)
	}
}

// TestWorkerSuiteRefusesAbsolutePathOutsideRoots is the worker-path statement
// of acceptance criterion 4: a worker execution's suite refuses an absolute
// path outside its worktree/project root, so the widening cannot leak.
func TestWorkerSuiteRefusesAbsolutePathOutsideRoots(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	abs := filepath.Join(outside, "escaped.txt")
	h := NewHostTools(dir, dir)
	ctx := context.Background()

	if _, err := h.Execute(ctx, "read", `{"path":`+hostJSONString(t, abs)+`}`); err == nil {
		t.Error("worker read accepted an absolute path outside its roots")
	}
	if _, err := h.Execute(ctx, "batch_read", `{"paths":[`+hostJSONString(t, abs)+`]}`); err == nil {
		t.Error("worker batch_read accepted an absolute path outside its roots")
	}
	if _, err := h.Execute(ctx, "write", `{"filePath":`+hostJSONString(t, abs)+`,"content":"x"}`); err == nil {
		t.Error("worker write accepted an absolute path outside its roots")
	}
	if _, err := os.Stat(abs); err == nil {
		t.Error("a file landed outside the worker's roots")
	}
}

// hostJSONString renders s as a JSON string literal for embedding in an args blob.
func hostJSONString(t *testing.T, s string) string {
	t.Helper()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
