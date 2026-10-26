package askorchicon

// consent_v2_golden_test.go — the opencode 2.x permission payload, pinned from a LIVE capture.
//
// REPORTED: "there is a bug with the opencode permissions cards. No matter what the directory. It seems
// to only print . and not the path." The card read
//
//	use the tool
//	· .
//
// The payloads in internal/opencode/testdata are the AUTHORITY (the code's own candidate keys were
// explicitly written to be corrected against a capture), and the 2.x one was taken from a live 2.0.27
// serve's event bus:
//
//	{"id":"per_…","sessionID":"ses_…","action":"edit","resources":["/tmp/orchicon-perm-probe2.txt"]}
//
// Every 1.x key this code looked for is absent from that payload, which is why nothing resolved and why
// the directory fell back to the (empty) scope directory and printed as ".".

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/scheduler"
)

// loadGoldenProps reads a captured `permission.asked` properties object.
func loadGoldenProps(t *testing.T, name string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "opencode", "testdata", name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	var props map[string]any
	if err := json.Unmarshal(raw, &props); err != nil {
		t.Fatalf("%s is not JSON: %v", name, err)
	}
	return props
}

func askEventFromGolden(t *testing.T, name string) scheduler.SessionEvent {
	t.Helper()
	props := loadGoldenProps(t, name)
	pid, _ := props["id"].(string)
	return scheduler.SessionEvent{Kind: "permission", PermissionID: pid, Detail: props}
}

// TestOpencode2AskResolvesItsToolAndPath is the regression pin for the reported card.
func TestOpencode2AskResolvesItsToolAndPath(t *testing.T) {
	a := extractAskAction(askEventFromGolden(t, "permission_asked_v2_golden.json"))

	if a.Tool != "edit" {
		t.Errorf("Tool = %q, want \"edit\" — 2.x spells the class `action`, and without it the card "+
			"renders its no-noun fallback and says %q", a.Tool, askSummary(a))
	}
	if len(a.Targets) != 1 || a.Targets[0] != "/tmp/orchicon-perm-probe2.txt" {
		t.Fatalf("Targets = %v, want the single resource path — 2.x spells paths `resources`, and "+
			"without it the card has no path at all", a.Targets)
	}
	// The card must NAME the path, not just hold it: "modify <path>" is what the operator reads, and the
	// reported symptom was a card with no noun.
	summary := askSummary(a)
	if !strings.Contains(summary, "/tmp/orchicon-perm-probe2.txt") {
		t.Errorf("summary = %q, want it to name the path", summary)
	}
	if strings.Contains(summary, "use the tool") {
		t.Errorf("summary = %q — the no-noun fallback means nothing resolved", summary)
	}
	// A file edit keys on the TARGET's directory, so the offered session grant covers something real.
	if got := resolveAskKey(a, ""); got != "/tmp" {
		t.Errorf("grant key = %q, want /tmp — the directory whose consent is actually missing", got)
	}
}

// TestOpencode2ShellAskBecomesACommand — 2.x spells a shell ask `action:"shell"` with the COMMAND as its
// single resource. It must become a command, not a target path: a shell ask is decided on the command
// line, and `isBashAsk` is what routes it there.
func TestOpencode2ShellAskBecomesACommand(t *testing.T) {
	a := extractAskAction(scheduler.SessionEvent{
		Kind: "permission", PermissionID: "per_x",
		Detail: map[string]any{
			"id": "per_x", "action": "shell", "resources": []any{"git status --short"},
		},
	})
	if !isBashAsk(a.Tool) {
		t.Fatalf("Tool = %q, want it recognised as a shell ask", a.Tool)
	}
	if a.Command != "git status --short" {
		t.Errorf("Command = %q, want the resource as the command line", a.Command)
	}
	if len(a.Targets) != 0 {
		t.Errorf("Targets = %v — a command is not a path; leaving it here puts the shell line on the "+
			"card as a file target", a.Targets)
	}
}

// TestOpencode1AskStillResolves — the 1.x capture must keep working. The fix ADDS a vocabulary; it does
// not replace one, and the two serves are both in the field.
func TestOpencode1AskStillResolves(t *testing.T) {
	a := extractAskAction(askEventFromGolden(t, "permission_asked_golden.json"))
	if a.Tool != "edit" {
		t.Errorf("Tool = %q, want \"edit\" (1.x `permission`)", a.Tool)
	}
	if len(a.Targets) != 1 || a.Targets[0] != "/p/sibling/notes.md" {
		t.Fatalf("Targets = %v, want the ABSOLUTE metadata.filepath — the relative `patterns` entry "+
			"resolves against the serve's cwd, not the project", a.Targets)
	}
}

// TestAnMCPAskWithNoDetailStillFailsClosed — a host-suite ask carries `resources:["*"]` and nothing else,
// so it must resolve to NO target and stay a card. Reading `resources` must not turn the wildcard into a
// path we were never given.
func TestAnMCPAskWithNoDetailStillFailsClosed(t *testing.T) {
	a := extractAskAction(scheduler.SessionEvent{
		Kind: "permission", PermissionID: "per_y",
		Detail: map[string]any{"id": "per_y", "action": "orchicon_write", "resources": []any{"*"}},
	})
	if len(a.Targets) != 0 {
		t.Fatalf("Targets = %v — a wildcard resource is not a path", a.Targets)
	}
	if a.Tool != "orchicon_write" {
		t.Errorf("Tool = %q, want the action name: even without a path, the card can say WHAT is asking",
			a.Tool)
	}
	if askActionResolved(a) {
		t.Error("an ask with no target and no command must report UNRESOLVED — resolved is what lets " +
			"it ride the project/grant verdicts and proceed without a card")
	}
}

// TestAnUnresolvedAskIsNotOfferedAFakeDirectory — the other half of the reported card.
//
// The card read `· .` and offered "never ask again in .". That "." was not opencode's: it was
// filepath.Clean("") — a target-less ask keys on the SCOPE directory, an Ask conversation has no
// project, so the scope is empty and Clean turns it into ".". The operator was being asked to grant a
// directory whose only meaning was "the serve's own cwd".
//
// Nothing here can invent the path, so the card must stop claiming one: a blank directory makes the
// session row say there is no directory to grant, and grantStore already refuses a blank key, so no
// grant can be recorded against it either.
func TestAnUnresolvedAskIsNotOfferedAFakeDirectory(t *testing.T) {
	a := extractAskAction(scheduler.SessionEvent{
		Kind: "permission", PermissionID: "per_z",
		Detail: map[string]any{"id": "per_z", "action": "orchicon_write", "resources": []any{"*"}},
	})
	if askActionResolved(a) {
		t.Fatal("precondition: this ask has no target and no command")
	}
	// The key a resolved ask would carry, for contrast: with no target it degrades to the scope dir,
	// and an empty scope dir cleans to "." — the value that reached the card.
	if got := resolveAskKey(a, ""); got != "." {
		t.Fatalf("resolveAskKey with an empty scope = %q; this test exists because that degrades to "+
			"the \".\" the operator saw", got)
	}
	// And the store will not record a grant for a blank directory, which is what makes blanking it safe.
	var g grantStore
	g.Grant("conv-1", "")
	if g.Len("conv-1") != 0 {
		t.Fatal("a blank directory recorded a grant — blanking the key must be inert")
	}
}
