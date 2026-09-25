package askorchicon

// ask_file_scope_statement_test.go — THE PROMPT TELLS THE TRUTH ABOUT REACH, and it agrees with the tool layer.
//
// Two claims used to be wrong in the same direction. The prompt described the file/shell suite as scoped to a
// project directory, which an agent reads as "you are confined to that tree" — untrue of a host process that
// reaches the operator's whole filesystem, and the kind of untruth that costs turns (the agent discovers the
// boundary by probing it, or declines a path it was allowed to read). And nothing anywhere told the agent WHEN
// the suite asks: reads never do; writes and executions do unless they land in the conversation's own
// pre-approved project directory.
//
// The statement is ONE function (writeSuiteReachBlock) emitted through every mode's persona, so these tests
// iterate everyMode rather than naming one. The last test is the one the work item asks for by name: the
// PROMPT's project statement and the TOOL layer's resolved default directory must agree for an assigned AND an
// unassigned conversation — the same pairing conversation_project_test.go uses, extended from "is it named?" to
// "is what it CLAIMS about scope the same thing the tool layer resolved?".

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/askmode"
	"github.com/beardedparrott/orchicon/internal/db"
)

// promptFor renders the full per-turn prompt (mode persona + the conversation's project block) for a test.
func promptFor(t *testing.T, mode, convProject string) string {
	t.Helper()
	return buildSystemPrompt(mode, testAgentConfig(), testToolRegistry(), nil, true, nil, "", convProject)
}

// askRootEnvelope runs the boundary probe and returns its envelope — the tool layer's own answer about scope.
func askRootEnvelope(t *testing.T, ctx context.Context) map[string]string {
	t.Helper()
	out, err := (&Service{}).NativeAskTools().ExecuteAskTool(ctx, askFileRootToolName, `{}`)
	if err != nil {
		t.Fatalf("ask_file_root: %v", err)
	}
	var env map[string]string
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("ask_file_root envelope is not JSON: %v (%s)", err, out)
	}
	return env
}

// EVERY MODE STATES THE SCOPE AND THE REACH — the work item's first acceptance criterion, iterated over the
// roster so a mode added later is covered without anyone remembering to add it here.
//
// The write sentence is DERIVED, not hand-written (writeSuiteReachBlock reads askmode.Allows) and that is
// asserted here too: a mode that cannot write must be told the platform REFUSES the tools, because telling it
// "writes ask" would promise a consent that cannot unlock anything.
func TestEveryModeStatesTheScopeAndTheReach(t *testing.T) {
	dir := t.TempDir()
	convProject := fmt.Sprintf("This chat belongs to the project **Orchicon** (ID p1, status active), whose "+
		"directory is `%s`.\nIt is also this conversation's DEFAULT SCOPE and its PRE-APPROVED directory: a write or "+
		"an execution inside `%s` proceeds without asking.\n", dir, dir)
	for _, mode := range everyMode {
		p := promptFor(t, mode, convProject)
		lp := strings.ToLower(p)
		for _, want := range []string{
			"host process",
			"whole filesystem",
			"reads never ask",
			"writes and executions ask",
			"pre-approved",
			"no root",
		} {
			if !strings.Contains(lp, want) {
				t.Errorf("%s: the prompt never says %q — the agent is left to discover the suite's reach and its "+
					"consent rules by probing them", mode, want)
			}
		}
		if !strings.Contains(p, dir) {
			t.Errorf("%s: the conversation's project directory %q is not named by the scope statement, so \"the "+
				"project is pre-approved\" names nothing", mode, dir)
		}
		if askmode.Allows(mode, "write") {
			if strings.Contains(lp, "cannot write anywhere") {
				t.Errorf("%s: the prompt denies writing, but the mode's gate permits it — the statement has drifted "+
					"from the enforcement", mode)
			}
		} else if !strings.Contains(lp, "cannot write anywhere") {
			t.Errorf("%s: this mode cannot write at all, so the prompt must say the platform REFUSES the tools "+
				"rather than implying a consent could unlock them", mode)
		}
	}
}

// THE FALSE CLAIM IS GONE EVERYWHERE IT APPEARED, not just where it was loudest. The repo's convention is to fix
// the class, and the prompt is assembled from more than one file, so this reads the SOURCES of the prompt half
// and fails if the assignment comes back anywhere in them.
func TestTheFalseScopeClaimIsGoneEverywhere(t *testing.T) {
	banned := []string{"first active project", "scoped to the tenant's"}
	for _, f := range []string{"agent.go", "chat.go"} {
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("cannot read %s: %v", f, err)
		}
		src := strings.ToLower(string(data))
		for _, b := range banned {
			if strings.Contains(src, b) {
				t.Errorf("%s claims %q — the suite is NOT scoped to the tenant's first active project (its root is "+
					"the conversation's project, and it reaches the whole filesystem)", f, b)
			}
		}
	}
}

// AN UNASSIGNED CHAT IS NEVER PRESENTED AS SCOPED. It has no project, so nothing is pre-approved and every write
// asks — and the tenant-wide anchor it still gets for relative paths must not be dressed up as its own workspace.
func TestUnassignedIsNeverPresentedAsScoped(t *testing.T) {
	anchor := "/tmp/tenant-wide-anchor-that-must-not-leak"
	for _, mode := range everyMode {
		p := promptFor(t, mode, "")
		lp := strings.ToLower(p)
		for _, want := range []string{
			"not assigned to a project",
			"no project",
			"outside",
			"ask",
			"ask_file_root",
			"nothing is pre-approved",
		} {
			if !strings.Contains(lp, want) {
				t.Errorf("%s: an unassigned chat's prompt never says %q, so it can be read as merely unlabelled "+
					"rather than unscoped", mode, want)
			}
		}
		if strings.Contains(p, anchor) {
			t.Errorf("%s: the tenant-wide fallback anchor %q leaked into the prompt as though it were this chat's "+
				"directory — the fallback anchors relative paths, it is not a project of this conversation's own",
				mode, anchor)
		}
		if strings.Contains(lp, "is the folder this conversation's work happens in") {
			t.Errorf("%s: the prompt gives the unassigned chat the ASSIGNED chat's wording, which asserts ownership "+
				"this conversation does not have", mode)
		}
	}
}

// THE PROMPT'S PROJECT STATEMENT AND THE TOOL LAYER'S RESOLVED DIRECTORY AGREE — for an assigned conversation and
// for an unassigned one, driven from ONE fixture value per case so the two halves cannot be satisfied by two
// different directories. The prompt half is buildSystemPrompt's; the tool half is ask_file_root's envelope, the
// same probe the agent is told to call.
func TestPromptStatementAgreesWithTheToolScope(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	// ASSIGNED: the tool layer resolves the conversation's own project, so its scope word is `conversation` and
	// the directory it names is pre-approved — which is exactly what the prompt must claim.
	restore := askFileRootStub(func(_ context.Context, _ *db.Pool) (AskFileScope, error) {
		return AskFileScope{Dir: dir, ProjectID: "p1", FromConversation: true}, nil
	})
	defer restore()
	env := askRootEnvelope(t, ctx)
	if env["scope"] != askScopeConversation {
		t.Fatalf("assigned conversation: ask_file_root scope = %q, want %q", env["scope"], askScopeConversation)
	}
	resolved := AskFileScope{Dir: env["project_dir"], ProjectID: env["project_id"], FromConversation: true}
	if !resolved.PreApprovedPath(resolved.Dir) {
		t.Fatalf("the tool layer did not pre-approve the conversation's own project directory %q", resolved.Dir)
	}
	convProject := fmt.Sprintf("This chat belongs to the project **Orchicon** (ID %s, status active), whose "+
		"directory is `%s`.\nIt is also this conversation's DEFAULT SCOPE and its PRE-APPROVED directory.\n",
		resolved.ProjectID, resolved.Dir)
	for _, mode := range everyMode {
		prompt := promptFor(t, mode, convProject)
		if !strings.Contains(prompt, resolved.Dir) {
			t.Errorf("%s: the prompt names a project statement that does not contain the directory the tool layer "+
				"resolved (%q) — the two halves have drifted", mode, resolved.Dir)
		}
	}

	// UNASSIGNED: a directory still resolves (the relative-path anchor) but NOTHING is approved, and the prompt
	// must claim exactly that — the trap this pins is the prompt calling the anchor the chat's scope while the
	// tool layer hands back `tenant_fallback`.
	restoreAnchor := askFileRootStub(func(_ context.Context, _ *db.Pool) (AskFileScope, error) {
		return AskFileScope{Dir: dir}, nil
	})
	defer restoreAnchor()
	env = askRootEnvelope(t, ctx)
	if env["scope"] != askScopeTenantAnchor {
		t.Fatalf("unassigned conversation: ask_file_root scope = %q, want %q", env["scope"], askScopeTenantAnchor)
	}
	if _, ok := env["project_id"]; ok {
		t.Fatalf("an unassigned conversation reported a project_id: %v", env)
	}
	anchorScope := AskFileScope{Dir: env["project_dir"]}
	if anchorScope.PreApprovedPath(anchorScope.Dir) {
		t.Fatalf("the tenant-wide anchor %q was treated as pre-approved — an unassigned chat must ask for every "+
			"write, even with a directory to anchor relative paths", anchorScope.Dir)
	}
	for _, mode := range everyMode {
		prompt := promptFor(t, mode, "")
		if !strings.Contains(strings.ToLower(prompt), "nothing is pre-approved") {
			t.Errorf("%s: the tool layer says this conversation approved nothing, but the prompt does not — the two "+
				"halves disagree about the consent decision", mode)
		}
	}
}

// THE PREDICATE THE CONSENT CORE READS. Inside the conversation's project ⇒ no ask; a sibling project's tree ⇒
// ask; and with FromConversation false there is nothing to approve at all, Dir itself included.
func TestPreApprovedPathPredicate(t *testing.T) {
	dir := "/home/me/projects/Orchicon"
	s := AskFileScope{Dir: dir, ProjectID: "p1", FromConversation: true}

	for _, target := range []string{
		dir,
		dir + "/",
		filepath.Join(dir, "internal", "askorchicon", "agent.go"),
		"agent.go",
		"./agent.go",
		"internal/askorchicon/agent.go",
	} {
		if !s.PreApprovedPath(target) {
			t.Errorf("PreApprovedPath(%q) = false, want true — a write inside the conversation's project must not "+
				"ask the user", target)
		}
	}

	for _, target := range []string{
		"/home/me/projects/Other/internal/x.go",
		filepath.Join(dir, "..", "Other", "x.go"),
		dir + "-v2/x.go", // shares the prefix without being inside
		"/",
		"/etc/passwd",
		"/tmp",
	} {
		if s.PreApprovedPath(target) {
			t.Errorf("PreApprovedPath(%q) = true, want false — a write outside the conversation's project (a sibling "+
				"project's tree included) must ask the user", target)
		}
	}

	// Nothing from the conversation ⇒ nothing approved, including the anchor itself.
	anchor := AskFileScope{Dir: dir}
	for _, target := range []string{dir, filepath.Join(dir, "a.go"), "a.go"} {
		if anchor.PreApprovedPath(target) {
			t.Errorf("a tenant-fallback scope approved %q — an unassigned conversation's anchor is not its project", target)
		}
	}
	if (AskFileScope{}).PreApprovedPath(dir) {
		t.Error("an empty scope approved a path; failing closed is the only safe direction")
	}
	if s.PreApprovedPath("") || s.PreApprovedPath("   ") {
		t.Error("an empty target was approved; failing closed is the only safe direction")
	}
}
