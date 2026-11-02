package claude

// hook_command_validate_test.go — a hook command that a shell would mis-split must fail at STARTUP.
//
// REPORTED as a non-durable permission grant: "run 1 refused every write, run 2 wrote everything, and
// permission-policy.yaml on disk is unchanged (deny list only, accept: [])". There is no stored grant to be
// non-durable — nothing in the codebase writes an accept list during a run — so the difference between those
// runs was whether the hook RAN. A failing PreToolUse hook blocks every gated tool, and the os.Stat checks in
// PreflightHookExecutable cannot see it: the FILE is perfect and only the COMMAND is wrong.
//
// The fix is a claim about the command itself, checked before the session starts.

import (
	"os"
	"path/filepath"
	"testing"
)

func osWriteFile(path string, b []byte, perm os.FileMode) error { return os.WriteFile(path, b, perm) }

// THE REPORTED SHAPE: an unquoted Windows install path.
func TestUnquotedPathWithSpaceIsRejected(t *testing.T) {
	cmd := `C:\Program Files\Orchicon\orchicon.exe claude-hook`
	if err := validateHookCommand(cmd); err == nil {
		t.Fatal("an UNQUOTED path with a space was accepted — a shell splits it at the first space, the hook " +
			"never runs, and claude blocks every gated tool with no reason an operator can act on")
	}
}

// The rendered form (what BuildSettings emits) is accepted, on both platforms.
func TestRenderedHookCommandIsAccepted(t *testing.T) {
	for _, bin := range []string{
		`C:\Program Files\Orchicon\orchicon.exe`,
		`C:\Users\justi\AppData\Local\Orchicon\orchicon.exe`,
		"/usr/local/bin/orchicon",
		"orchicon",
	} {
		if err := validateHookCommand(hookCommand(bin)); err != nil {
			t.Errorf("rendered command for %q was rejected: %v", bin, err)
		}
	}
}

// A command that would run something other than the hook subcommand is refused too.
func TestWrongSubcommandIsRejected(t *testing.T) {
	for _, cmd := range []string{
		`"/bin/orchicon"`,
		`"/bin/orchicon" serve`,
		`"/bin/orchicon" claude-hook --extra`,
		`"" claude-hook`,
	} {
		if err := validateHookCommand(cmd); err == nil {
			t.Errorf("validateHookCommand(%q) = nil, want a rejection", cmd)
		}
	}
}

// The splitter must group on double quotes the way a shell does — otherwise the check above would reject the
// very form it exists to require.
func TestHookCommandSplitterGroupsQuotedSpans(t *testing.T) {
	got := splitHookCommand(`"C:\Program Files\x.exe" claude-hook`)
	if len(got) != 2 {
		t.Fatalf("split into %d tokens (%q), want 2", len(got), got)
	}
	if got[0] != `C:\Program Files\x.exe` || got[1] != "claude-hook" {
		t.Fatalf("split = %q, want the path and the subcommand", got)
	}
	// And the unquoted form splits into more, which is exactly what the validator catches.
	if n := len(splitHookCommand(`C:\Program Files\x.exe claude-hook`)); n != 3 {
		t.Fatalf("unquoted split into %d tokens, want 3 (path broke at the space)", n)
	}
}

// THE END-TO-END CASE FOR THE REPORTED HOST: a binary whose path CONTAINS A SPACE must still preflight
// clean, because the renderer quotes it. This is the assertion that would have failed before the quoting fix
// and passes after it — the preflight now checks the RENDERED command, so a path with a space is proven
// usable rather than assumed usable.
func TestPreflightAcceptsABinaryPathWithASpace(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "Program Files")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	bin := filepath.Join(dir, "orchicon.exe")
	if err := writeExecutable(bin); err != nil {
		t.Fatalf("write fake binary: %v", err)
	}
	t.Setenv(HookBinEnv, bin)

	if err := PreflightHookExecutable(false); err != nil {
		t.Fatalf("a hook binary at %q was rejected: %v\n  the renderer quotes the path, so this is exactly "+
			"the shape that must work", bin, err)
	}
	// And the UNQUOTED form of that same path is the defect: the validator must reject it, proving the
	// preflight is checking a real property rather than passing everything it is handed.
	if err := validateHookCommand(bin + " claude-hook"); err == nil {
		t.Fatalf("an UNQUOTED path with a space was accepted: %s", bin)
	}
}

// writeExecutable creates a file that passes the preflight's stat checks.
func writeExecutable(path string) error {
	return osWriteFile(path, []byte("#!/bin/sh\n"), 0o755)
}
