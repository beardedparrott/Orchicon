package adapter

// adapterpath_test.go — AN ADAPTER IS WHEREVER IT IS, AND A CONTAINER NEEDS A BINARY ITS KERNEL CAN RUN.
//
// Two reported failures drive these tests, and they are different problems that look the same from the
// operator's chair:
//
//	claude    C:\Users\<user>\AppData\Local\Microsoft\WinGet\Packages\
//	              Anthropic.ClaudeCode_Microsoft.Winget.Source_8wekyb3d8bbwe\claude.exe
//	opencode  C:\Users\<user>\AppData\Roaming\npm\opencode.ps1
//
// 1. THE HOST RESOLVER looked in one declared place, so a WinGet package or an npm global install was
//    "not found" — and the bare-name fallback could not save it, because the plane's PATH is the one it
//    started with.
// 2. EVEN WHEN IT RESOLVES, a Windows .exe cannot execute in a Debian container. Mounting "the adapter"
//    and inheriting PATH is not enough: the binary must be runnable BY THE CONTAINER'S KERNEL.
//
// These live in internal/adapter because BOTH sides must read them: internal/runtime to derive mounts and
// PATH, and each adapter package to resolve its host binary. Two lists is what made them disagree.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakes builds a temp tree shaped like a Windows user profile, plus a getenv over it.
type winTree struct {
	root   string
	vars   map[string]string
	onPath map[string]string
}

func newWinTree(t *testing.T) *winTree {
	t.Helper()
	root, err := os.MkdirTemp(os.TempDir(), "adapterpath-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	return &winTree{root: root, vars: map[string]string{
		"USERPROFILE":  root,
		"LOCALAPPDATA": filepath.Join(root, "AppData", "Local"),
		"APPDATA":      filepath.Join(root, "AppData", "Roaming"),
		"ProgramFiles": filepath.Join(root, "Program Files"),
	}}
}

func (w *winTree) getenv(k string) string { return w.vars[k] }

// file creates a file (and parents) under the tree root and returns its path.
func (w *winTree) file(t *testing.T, parts ...string) string {
	t.Helper()
	p := filepath.Join(append([]string{w.root}, parts...)...)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("MZ"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestResolvesTheOperatorsClaudeInstall is the reported path, exactly: a WinGet PACKAGE directory, whose name
// embeds a source and a hash and therefore cannot be spelled out.
func TestResolvesTheOperatorsClaudeInstall(t *testing.T) {
	w := newWinTree(t)
	want := w.file(t, "AppData", "Local", "Microsoft", "WinGet", "Packages",
		"Anthropic.ClaudeCode_Microsoft.Winget.Source_8wekyb3d8bbwe", "claude.exe")

	if got := ResolveBinary("windows", w.root, KindClaude, w.getenv); got != want {
		t.Fatalf("ResolveBinary(claude) = %q, want the WinGet package binary %q", got, want)
	}
}

// TestResolvesOpencodeFromAnNpmGlobalInstall covers the other reported shape: npm's prefix holds a .ps1 SHIM,
// and the real executable is one level down in node_modules.
func TestResolvesOpencodeFromAnNpmGlobalInstall(t *testing.T) {
	w := newWinTree(t)
	npm := filepath.Join(w.root, "AppData", "Roaming", "npm")
	if err := os.MkdirAll(npm, 0o755); err != nil {
		t.Fatal(err)
	}
	// The shim the operator actually has — it must NEVER be the answer.
	for _, name := range []string{"opencode.ps1", "opencode.cmd", "opencode"} {
		if err := os.WriteFile(filepath.Join(npm, name), []byte("@echo off"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	want := w.file(t, "AppData", "Roaming", "npm", "node_modules", "opencode-ai", "bin", "opencode.exe")

	got := ResolveBinary("windows", w.root, DefaultAdapterKind, w.getenv)
	if got != want {
		t.Fatalf("ResolveBinary(opencode) = %q, want the node_modules binary %q — a shim is not a binary",
			got, want)
	}
}

// TestNeverResolvesAShim pins the trap: Go cannot execute a .cmd/.bat/.ps1, so resolving one turns a clean
// "not found" into a dispatch that dies where the operator has no context.
func TestNeverResolvesAShim(t *testing.T) {
	w := newWinTree(t)
	npm := filepath.Join(w.root, "AppData", "Roaming", "npm")
	if err := os.MkdirAll(npm, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(npm, "opencode.ps1"), []byte("@echo off"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := ResolveBinary("windows", w.root, DefaultAdapterKind, w.getenv); got != "" {
		t.Fatalf("resolved the shim %q — nothing should resolve, and the hint should name the shim", got)
	}
	hint := ShimHint("windows", w.root, DefaultAdapterKind, w.getenv)
	for _, want := range []string{"opencode.ps1", "SHIM", BinEnv(DefaultAdapterKind)} {
		if !strings.Contains(hint, want) {
			t.Errorf("hint must mention %q so 'not found' is not a lie; got: %s", want, hint)
		}
	}
}

// TestOverrideWins keeps the documented escape hatch first, because every error message points at it.
func TestOverrideWins(t *testing.T) {
	w := newWinTree(t)
	w.file(t, "AppData", "Local", "Microsoft", "WinGet", "Packages", "Anthropic.ClaudeCode_X", "claude.exe")
	w.vars[BinEnv(KindClaude)] = `D:\tools\claude.exe`
	if got := ResolveBinary("windows", w.root, KindClaude, w.getenv); got != `D:\tools\claude.exe` {
		t.Errorf("override ignored: got %q", got)
	}
}

// TestPosixIsUnchanged is the regression guard: on Linux and macOS nothing about the Windows search runs.
func TestPosixIsUnchanged(t *testing.T) {
	home := t.TempDir()
	declared := filepath.Join(home, ".opencode", "bin", "opencode")
	if err := os.MkdirAll(filepath.Dir(declared), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(declared, []byte("ELF"), 0o755); err != nil {
		t.Fatal(err)
	}
	env := func(string) string { return "" }
	if got := ResolveBinary("linux", home, DefaultAdapterKind, env); got != declared {
		t.Errorf("linux resolution = %q, want the declared dir %q", got, declared)
	}
	if got := ShimHint("linux", home, DefaultAdapterKind, env); got != "" {
		t.Errorf("linux produced a Windows shim hint: %q", got)
	}
}

// TestWindowsPathsAreAbsoluteAnywhere guards the bug where a relative candidate is "found" against whatever
// directory the process happens to be running in.
func TestWindowsPathsAreAbsoluteAnywhere(t *testing.T) {
	w := newWinTree(t)
	want := w.file(t, "AppData", "Local", "Microsoft", "WinGet", "Packages", "Anthropic.ClaudeCode_X", "claude.exe")
	got := ResolveBinary("windows", w.root, KindClaude, w.getenv)
	if !filepath.IsAbs(got) {
		t.Fatalf("resolved a relative path %q", got)
	}
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// --- the container half ---------------------------------------------------------------------------

// TestAWindowsExeIsNotUsableInAContainer is the check that stops a container starting with an adapter it
// cannot run. This is the failure that would otherwise appear INSIDE the container, far from the cause.
func TestAWindowsExeIsNotUsableInAContainer(t *testing.T) {
	ok, why := ContainerUsable(`C:\Users\x\.local\bin\claude.exe`, "linux")
	if ok {
		t.Fatal("a Windows .exe was reported usable inside a Linux container")
	}
	for _, want := range []string{"Windows executable", "Linux", BinEnv(KindClaude), "ORCHICON_OPENCODE_BIN"} {
		if !strings.Contains(why, want) {
			t.Errorf("the refusal must name %q — the operator needs the remedy, not just the verdict; got: %s",
				want, why)
		}
	}
}

// TestALinuxBinaryIsUsable keeps the working case working: an adapter installed for the container's kernel.
func TestALinuxBinaryIsUsable(t *testing.T) {
	for _, p := range []string{
		"/home/op/.opencode/bin/opencode",
		"/usr/local/bin/claude",
		"/mnt/c/Users/op/.opencode/bin/opencode", // a /mnt path is still a LINUX path to the kernel
	} {
		if ok, why := ContainerUsable(p, "linux"); !ok {
			t.Errorf("%s reported unusable: %s", p, why)
		}
	}
}

// TestNoResolvedAdapterIsItsOwnMessage pins that "nothing resolved" is distinguished from "the wrong kind of
// binary" — they have different remedies (install it, versus install the LINUX build).
func TestNoResolvedAdapterIsItsOwnMessage(t *testing.T) {
	ok, why := ContainerUsable("", "linux")
	if ok {
		t.Fatal("an empty path was reported usable")
	}
	if strings.Contains(why, "Windows executable") {
		t.Errorf("an empty path must not be described as a Windows binary: %s", why)
	}
	if !strings.Contains(why, "resolved") {
		t.Errorf("the message must say nothing was resolved: %s", why)
	}
}
