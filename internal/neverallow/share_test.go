package neverallow_test

// share_test.go — THE SHARING PROOF.
//
// The acceptance criterion is that the execution guard and the opencode config
// consume the SAME declaration, "not two lists that agree today". A test that
// compared two independently written lists would pass after they diverged in
// intent (the lists agree until someone edits one). So the proof is MUTATION:
// a sentinel appended to the shared declaration must show up in BOTH consumers'
// output without either of them being touched.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/claude"
	"github.com/beardedparrott/orchicon/internal/guard"
	"github.com/beardedparrott/orchicon/internal/neverallow"
	"github.com/beardedparrott/orchicon/internal/opencode"
)

// The guard's shim is RENDERED from the shared declaration: add a member to the
// binary class and it appears in the generated `case` arm AND gets a symlink.
func TestGuardConsumesTheSharedBinaryClass(t *testing.T) {
	const sentinel = "orchicon-neverallow-sentinel"
	orig := append([]neverallow.Binary(nil), neverallow.Binaries...)
	neverallow.Binaries = append(neverallow.Binaries, neverallow.Binary{Name: sentinel, Shim: true})
	t.Cleanup(func() { neverallow.Binaries = orig })

	dir, err := guard.MakeGuard(t.TempDir(), "")
	if err != nil {
		t.Fatalf("make guard: %v", err)
	}
	defer os.RemoveAll(dir)

	script, err := os.ReadFile(filepath.Join(dir, "guard"))
	if err != nil {
		t.Fatalf("read generated guard script: %v", err)
	}
	if !strings.Contains(string(script), sentinel) {
		t.Errorf("the generated guard shim does not carry %q — the guard does not render its case arm from neverallow", sentinel)
	}
	if _, err := os.Lstat(filepath.Join(dir, sentinel)); err != nil {
		t.Errorf("the guard did not shim %q — it does not build its shim set from neverallow: %v", sentinel, err)
	}
}

// The config's bash deny map is BUILT from the shared declaration — in BOTH
// profiles, so a member cannot be dropped from one of them by accident.
func TestConfigProfilesConsumeTheSharedCommandClass(t *testing.T) {
	const sentinel = "orchicon-neverallow-sentinel *"
	orig := append([]string(nil), neverallow.CommandPatterns...)
	neverallow.CommandPatterns = append(neverallow.CommandPatterns, sentinel)
	t.Cleanup(func() { neverallow.CommandPatterns = orig })

	profiles := []opencode.PermissionProfile{opencode.ProfileWorker, opencode.ProfileInteractive}
	for _, p := range profiles {
		bash, ok := opencode.PermissionRulesForProfile(p)["bash"].(map[string]any)
		if !ok {
			t.Fatalf("profile %q has no bash rule map", p)
		}
		if got, ok := bash[sentinel].(string); !ok || got != "deny" {
			t.Errorf("profile %q: bash[%q] = %#v, want \"deny\" — the config does not read neverallow.CommandPatterns", p, sentinel, bash[sentinel])
		}
	}
}

// The CLAUDE worker adapter consumes the shared command class too: a sentinel
// appended to the declaration must show up in the PreToolUse hook's refusal for
// that command, with no edit to internal/claude. This is what makes the claude
// worker's restrictions and the opencode worker's restrictions ONE declaration
// rather than two that agree today.
func TestClaudeHookConsumesTheSharedCommandClass(t *testing.T) {
	const sentinel = "orchicon-neverallow-sentinel *"
	orig := append([]string(nil), neverallow.CommandPatterns...)
	neverallow.CommandPatterns = append(neverallow.CommandPatterns, sentinel)
	t.Cleanup(func() { neverallow.CommandPatterns = orig })

	// The opencode worker profile sees it...
	bash, ok := opencode.PermissionRulesForProfile(opencode.ProfileWorker)["bash"].(map[string]any)
	if !ok {
		t.Fatal("the opencode worker profile has no bash rule map")
	}
	if got, ok := bash[sentinel].(string); !ok || got != "deny" {
		t.Errorf("the opencode worker profile does not carry the sentinel: %#v", bash[sentinel])
	}

	// ...and so does the claude hook, for the same declaration.
	v := claude.DecideTool(claude.HookInput{
		ToolName:  "Bash",
		ToolInput: map[string]any{"command": "orchicon-neverallow-sentinel off"},
	}, t.TempDir(), "")
	if v.Allow {
		t.Fatal("the claude hook allowed a command in the shared never-allow class — it does not read the declaration")
	}
	if v.Rule != sentinel {
		t.Errorf("the claude hook refused by rule %q, want the sentinel %q", v.Rule, sentinel)
	}
}

// THE ANTI-DUPLICATION SCAN. Neither consumer may restate a member of the class
// as a literal — that is exactly the second list this package exists to remove.
// It scans for the QUOTED string forms (a Go string literal or a struct field),
// so the prose comments that legitimately name the class are not false hits.
func TestTheClassIsNotRestatedInItsConsumers(t *testing.T) {
	literals := make([]string, 0, len(neverallow.CommandPatterns)+len(neverallow.Binaries))
	for _, p := range neverallow.CommandPatterns {
		literals = append(literals, `"`+p+`"`)
	}
	for _, b := range neverallow.Binaries {
		if !strings.Contains(b.Name, "*") {
			literals = append(literals, `"`+b.Name+`"`)
		}
	}

	// BOTH worker-adapters' restriction layers are scanned: the claude launch
	// (permissions.go) and its PreToolUse decision core (hook.go) must consume the
	// declaration, never re-state it.
	for _, file := range []string{
		"../guard/guard.go",
		"../opencode/config.go",
		"../claude/permissions.go",
		"../claude/hook.go",
	} {
		src, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		body := string(src)
		for _, lit := range literals {
			if strings.Contains(body, lit) {
				t.Errorf("%s still restates the never-allow member %s — the class must live ONLY in internal/neverallow", file, lit)
			}
		}
	}
}
