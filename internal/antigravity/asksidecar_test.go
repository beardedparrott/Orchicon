package antigravity

// asksidecar_test.go — a REAL `orchicon` binary for the live probes that depend on the Orchicon MCP sidecar.
//
// WHY THIS FILE EXISTS (it is a defect fix, not tidying). `orchiconACPSServerForAsk` builds the platform's
// sidecar entry through `claude.OrchiconMCPServer(claude.HookBinaryFor(false), ...)`, and `HookBinaryFor`
// resolves that command from `claude.HookBinEnv`, falling back to `os.Executable()`. ON THE PLANE THAT IS
// CORRECT: the plug-in's own executable IS `orchicon`, so `orchicon mcp` is exactly the sidecar.
//
// INSIDE `go test` IT IS WRONG, AND SILENTLY SO: `os.Executable()` is the TEST binary, which has no `mcp`
// subcommand. agy spawns it, the handshake fails, the conversation gets no `orchicon_*` tools at all, and the
// model reports that it "cannot call orchicon_ask_user". `TestAskLive_AskUserToolReachesTheModel` therefore
// failed for a reason that had nothing to do with the code under test — and, before this file, an operator
// running the documented command saw a red probe that no product change could ever fix.
//
// Measured on 2026-11-26 (agy 1.5.13, this branch):
//   - documented command, sidecar = test binary  → 0 ask cards, tools seen = [], FAIL
//   - same test, sidecar = a built ./cmd/orchicon → tools seen = [mcp_orchicon_ask_user], PASS, 9.1s
//
// So the live probes take their sidecar from a REAL binary, and one that cannot be produced SKIPS with a
// message naming the missing prerequisite, rather than reporting a product failure it cannot attribute.

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/beardedparrott/orchicon/internal/claude"
)

var (
	askSidecarOnce sync.Once
	askSidecarPath string
	askSidecarErr  error
)

// askSidecarBinary returns an absolute path to a real `orchicon` binary and points `claude.HookBinEnv` at it,
// so any sidecar entry built after this call spawns THAT rather than the test binary.
//
// It prefers a binary the operator already pointed us at (`ORCHICON_TEST_BINARY`, the var
// cmd/orchicon's own sidecar test uses, then `ORCHICON_CLAUDE_HOOK_BIN`) and otherwise builds ./cmd/orchicon
// into a temp dir ONCE per test process. It SKIPS when neither is possible: a probe that cannot provision its
// prerequisite must not claim the product is broken.
func askSidecarBinary(t *testing.T) string {
	t.Helper()
	askSidecarOnce.Do(func() {
		for _, env := range []string{"ORCHICON_TEST_BINARY", claude.HookBinEnv} {
			v := strings.TrimSpace(os.Getenv(env))
			if v == "" {
				continue
			}
			if st, err := os.Stat(v); err == nil && !st.IsDir() {
				abs, aerr := filepath.Abs(v)
				if aerr != nil {
					abs = v
				}
				askSidecarPath = abs
				return
			}
		}

		root, err := filepath.Abs("../..")
		if err != nil {
			askSidecarErr = err
			return
		}
		bin := filepath.Join(t.TempDir(), "orchicon")
		// -o into a temp dir: never overwrites a tracked bin/, and never races a parallel package's build.
		cmd := exec.Command("go", "build", "-o", bin, "./cmd/orchicon")
		cmd.Dir = root
		if out, berr := cmd.CombinedOutput(); berr != nil {
			askSidecarErr = berr
			t.Logf("could not build ./cmd/orchicon for the MCP sidecar: %v\n%s", berr, out)
			return
		}
		askSidecarPath = bin
	})

	if askSidecarPath == "" {
		t.Skipf("cannot prove the ask card against the real agy: no `orchicon` binary for the MCP sidecar "+
			"(build failed: %v). Set ORCHICON_TEST_BINARY or %s to skip the build.", askSidecarErr, claude.HookBinEnv)
	}
	// Setenv LAST and every call: the bridge reads the env at session-new time, not at helper time.
	t.Setenv(claude.HookBinEnv, askSidecarPath)
	return askSidecarPath
}

// TestAskSidecarCommandIsNotTheTestBinary is the always-on guard for the defect above.
//
// It does not need agy, a model turn or a database: it asserts the ONE property whose absence made a live probe
// fail for a non-product reason — that after the helper runs, the sidecar entry points at a real executable
// file that is NOT this test process's own binary. Without this, the regression returns the moment anyone
// removes the helper call, and it returns as a mysterious live-only failure again.
func TestAskSidecarCommandIsNotTheTestBinary(t *testing.T) {
	bin := askSidecarBinary(t)

	b := New(quiet())
	srv := b.orchiconACPSServerForAsk("tnt_test", "conv_1")

	if srv.Name != "orchicon" {
		t.Fatalf("ask sidecar name = %q, want %q — the ask card is addressed by that name", srv.Name, "orchicon")
	}
	if srv.Command != bin {
		t.Errorf("ask sidecar command = %q, want the provisioned binary %q", srv.Command, bin)
	}
	if len(srv.Args) == 0 || srv.Args[0] != "mcp" {
		t.Errorf("ask sidecar argv = %v, want [\"mcp\"] — the sidecar is the `orchicon mcp` subcommand", srv.Args)
	}
	if self, err := os.Executable(); err == nil && srv.Command == self {
		t.Fatalf("ask sidecar command is THIS TEST BINARY (%s). `go test` binaries have no `mcp` subcommand, "+
			"so agy cannot start the sidecar, the conversation gets no orchicon_* tools, and every ask-card "+
			"probe fails for a reason no product change can fix. Point %s at a real orchicon.",
			self, claude.HookBinEnv)
	}
	if st, err := os.Stat(srv.Command); err != nil || st.IsDir() {
		t.Fatalf("ask sidecar command %q is not an executable file (err=%v): agy would fail to spawn it and "+
			"the ask card would be unreachable", srv.Command, err)
	}
}
