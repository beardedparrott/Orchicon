package claude

// ask_mcp_scope_test.go — the claude Ask half of child 6.
//
// WHAT IT PROVES (AC 4, AC 5's claude leg, and the mode policy's claude carrier):
//
//   - an Ask conversation resolves the CONVERSATION scope with NON-EMPTY ids — the
//     conversation's project AND the conversation itself — instead of the old
//     `{Kind: ScopeProject}` with nothing in it (the defect this closes);
//   - the resolved set reaches the `--mcp-config` claude is launched with;
//   - the mode file carries `MayAct`, and an opaque MCP tool is REFUSED with the
//     shared wording in a mode that may not act (read from the SAME table);
//   - an operator's MCP tool is ASK-BY-DEFAULT in every mode (pinned, not implied),
//     while the platform's own `mcp__orchicon__*` is allowed.

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/askmode"
	"github.com/beardedparrott/orchicon/internal/mcpclient"
)

// askScopeResolver captures every ref it was asked for and answers with a fixed
// resolution.
type askScopeResolver struct {
	res mcpclient.Resolution
	got []mcpclient.ScopeRef
}

func (r *askScopeResolver) ResolveScope(_ context.Context, ref mcpclient.ScopeRef) (mcpclient.Resolution, error) {
	r.got = append(r.got, ref)
	return r.res, nil
}

// AC 4: an Ask conversation resolves the CONVERSATION scope with the project AND
// conversation ids — not the empty old ref — and both the project's and the
// conversation's servers reach the argv.
func TestClaudeAskResolvesTheConversationScopeWithRealIDs(t *testing.T) {
	builtinFor(t) // installs the fake CLI + HOME
	resolver := &askScopeResolver{res: mcpclient.Resolution{
		Servers: []mcpclient.ScopedServer{
			{Spec: mcpclient.ServerSpec{ID: "proj-srv", Type: mcpclient.TypeStdio, Command: []string{"/bin/proj"}},
				From: mcpclient.ScopeProject, FromID: "project:p1", EntryID: "proj-srv"},
			{Spec: mcpclient.ServerSpec{ID: "conv-srv", Type: mcpclient.TypeStdio, Command: []string{"/bin/conv"}},
				From: mcpclient.ScopeConversation, FromID: "conversation:c1", EntryID: "conv-srv"},
		},
		SelectedIDs: []string{"proj-srv", "conv-srv"},
	}}
	b := New(quietLogger())
	b.SetScopeResolver(resolver)

	s := newAskSession(b, "c1", filepath.Join(t.TempDir(), "ask"))
	s.tenantID = "tnt_dev"
	s.projectID = "p1"

	argv := s.argv()
	// The ref carried BOTH ids — the whole point of AC 4.
	if len(resolver.got) == 0 {
		t.Fatal("the resolver was never consulted")
	}
	last := resolver.got[len(resolver.got)-1]
	if last.Kind != mcpclient.ScopeConversation {
		t.Errorf("resolved scope = %q, want conversation", last.Kind)
	}
	if last.ProjectID != "p1" || last.ConversationID != "c1" {
		t.Errorf("resolved ref = %+v, want project p1 conversation c1 — empty ids are the defect this closes", last)
	}

	cfg := mcpConfigArg(t, argv)
	for _, want := range []string{`"orchicon"`, `"proj-srv"`, `"conv-srv"`} {
		if !strings.Contains(cfg, want) {
			t.Errorf("the rendered config is missing %q: %s", want, cfg)
		}
	}
}

// mcpConfigArg extracts the inline --mcp-config document from an Ask argv.
func mcpConfigArg(t *testing.T, argv []string) string {
	t.Helper()
	for i, a := range argv {
		if a == "--mcp-config" && i+1 < len(argv) {
			return argv[i+1]
		}
	}
	t.Fatal("the Ask argv has no --mcp-config")
	return ""
}

// THE SIDECAR IS A CHILD PROCESS, SO THE SCOPE MUST RIDE ITS ENVIRONMENT.
//
// The turn's conversation and project are stamped on the CONTEXT in-process, which is
// fine for the native Ask path (its tools run in this process) and impossible to pass to
// a stdio child. The claude child is per conversation with an argv fixed at spawn, so its
// environment is the one channel that cannot go stale — and without it every Orchicon
// tool in a claude Ask turn ran on an unstamped context, which is why the operator saw
// get_current_conversation fail with "no conversation is stamped on this turn".
func TestClaudeAskHandsTheConversationScopeToTheSidecar(t *testing.T) {
	builtinFor(t)
	b := New(quietLogger())
	b.SetScopeResolver(&askScopeResolver{})

	s := newAskSession(b, "c-env", filepath.Join(t.TempDir(), "ask"))
	s.tenantID = "tnt_dev"
	s.projectID = "p-env"

	cfg := mcpConfigArg(t, s.argv())
	for _, want := range []string{
		`"ORCHICON_MCP_CONVERSATION_ID":"c-env"`,
		`"ORCHICON_MCP_CONVERSATION_PROJECT_ID":"p-env"`,
		`"ORCHICON_MCP_TENANT_ID":"tnt_dev"`,
	} {
		if !strings.Contains(cfg, want) {
			t.Errorf("the sidecar's environment is missing %s — its tools would answer \"no conversation is stamped on this turn\": %s", want, cfg)
		}
	}
}

// A conversation with NO project still sends the conversation id, and omits only the
// project var. The sidecar then stamps the scope's own meaning for "" — "assigned to no
// project" — rather than leaving the whole scope unset, which would make the session
// look like it had no conversation at all.
func TestClaudeAskConversationEnvOmitsAnAbsentProject(t *testing.T) {
	env := OrchiconMCPConversationEnv("c-only", "")
	if env[MCPConversationEnv] != "c-only" {
		t.Errorf("env = %+v, want the conversation id", env)
	}
	if _, ok := env[MCPConversationProjectEnv]; ok {
		t.Errorf("env = %+v, want NO project var for an unassigned conversation", env)
	}

	// And a transport with no conversation (a worker sidecar) sends nothing at all,
	// so the sidecar leaves the context unstamped instead of inventing one.
	if got := OrchiconMCPConversationEnv("", "p-worker"); got != nil {
		t.Errorf("OrchiconMCPConversationEnv(\"\", ...) = %+v, want nil — a worker sidecar must not claim a conversation", got)
	}
}

// The mode file ROUND-TRIPS MayAct, written from the SAME table.
func TestAskModeFileCarriesMayAct(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "c1.mode.json")
	if err := writeAskModeFile(path, askmode.Iteration); err != nil {
		t.Fatalf("write iteration: %v", err)
	}
	st, ok := readAskModeFile(path)
	if !ok {
		t.Fatal("the iteration mode file did not read back")
	}
	if !st.MayAct {
		t.Error("iteration's mode file says MayAct=false — it is the ONE mode that may act")
	}
	if err := writeAskModeFile(path, askmode.Brainstorm); err != nil {
		t.Fatalf("write brainstorm: %v", err)
	}
	st, ok = readAskModeFile(path)
	if !ok {
		t.Fatal("the brainstorm mode file did not read back")
	}
	if st.MayAct {
		t.Error("brainstorm's mode file says MayAct=true — it does not act")
	}
}

// AC 5 (claude leg): an opaque MCP tool is refused in a mode that may not act, with
// the SAME wording as the native adapter — names the mode, says the model cannot
// switch itself.
func TestAskModeDenialRefusesOpaqueMCPOutsideAModeThatActs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "c1.mode.json")
	for _, mode := range []string{askmode.Brainstorm, askmode.QuickWork} {
		if err := writeAskModeFile(path, mode); err != nil {
			t.Fatalf("write %s: %v", mode, err)
		}
		denied, reason := askModeDenial(path, "mcp__github__create_issue")
		if !denied {
			t.Errorf("%s did not refuse an opaque MCP tool — a planning session can act", mode)
			continue
		}
		for _, want := range []string{mode, "REFUSED BY THE PLATFORM", "switch your own mode", "ASK THE USER"} {
			if !strings.Contains(reason, want) {
				t.Errorf("%s refusal is missing %q: %s", mode, want, reason)
			}
		}
	}
	// ITERATION MAY ACT: the opaque tool is NOT refused by the mode gate (consent
	// gates it instead — a separate layer).
	if err := writeAskModeFile(path, askmode.Iteration); err != nil {
		t.Fatalf("write iteration: %v", err)
	}
	if denied, _ := askModeDenial(path, "mcp__github__create_issue"); denied {
		t.Error("iteration refused an opaque MCP tool at the MODE gate — the mode may act; consent is the gate that applies")
	}
}

// The platform's OWN server stays classified by the table: `mcp__orchicon__create_work_item`
// is a PLANNER tool, denied in Iteration (which may not author the plan) — the prefix
// stripping is what keeps the table honest for the sidecar.
func TestAskModeDenialClassifiesThePlatformsOwnServer(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "c1.mode.json")
	if err := writeAskModeFile(path, askmode.Iteration); err != nil {
		t.Fatalf("write: %v", err)
	}
	denied, reason := askModeDenial(path, "mcp__orchicon__create_work_item")
	if !denied {
		t.Fatal("iteration did not refuse the platform's create_work_item — the bare-name classification through the prefix is missing")
	}
	if !strings.Contains(strings.ToUpper(reason), "BRAINSTORM") {
		t.Errorf("the refusal does not name the mode to switch to: %s", reason)
	}
}

// EVERY MCP TOOL IS ALLOWED BY THE HOOK, the operator's as well as the platform's — and it must be an
// explicit ALLOW rather than "no verdict", because abstaining would hand the call to the permission
// system and prompt anyway (the same card by the other route).
//
// THE REQUIREMENT FLIPPED. This used to require the opposite for a third-party server, on the
// reasoning that an opaque tool is where a human decision is worth having. The operator overruled it:
// "I didn't even think MCP was supposed to have a card. If it's added in scope it should just have
// access tbh. I don't want cards for that." The server is in this session only because they attached
// it, so the decision was already made.
//
// THE MODE BOUNDARY IS UNAFFECTED and is the reason this is safe: RunHook consults askModeDenial
// BEFORE DecideToolForAsk, so an opaque MCP tool is still REFUSED in Brainstorm and Quick Work. The
// next test in this file pins exactly that half.
func TestDecideToolForAskAllowsEveryMCPTool(t *testing.T) {
	h := HookInput{ToolName: "mcp__github__create_issue", ToolInput: map[string]any{}}
	if v := DecideToolForAsk(h, t.TempDir(), ""); !v.Allow {
		t.Errorf("an operator's MCP tool was not ALLOWED: %+v — it must not card, and it must not merely "+
			"abstain either (abstaining leaves the permission system to prompt)", v)
	}
	h = HookInput{ToolName: "mcp__orchicon__get_current_conversation", ToolInput: map[string]any{}}
	if v := DecideToolForAsk(h, t.TempDir(), ""); !v.Allow {
		t.Errorf("the platform's own mcp__orchicon__* tool was not allowed: %+v — asking would put a card in front of a session reading its own record", v)
	}
}

// AC 5's OFFERED half for claude: a mode that may not act is not OFFERED the
// operator's opaque MCP servers at all — the `--mcp-config` carries only the
// platform's own sidecar. The hook denies the call too (the ENFORCED half), but
// advertising an action the mode refuses would invite the model to try it.
func TestClaudeAskWithholdsOperatorMCPOutsideAModeThatActs(t *testing.T) {
	builtinFor(t)
	resolver := &askScopeResolver{res: mcpclient.Resolution{
		Servers: []mcpclient.ScopedServer{
			{Spec: mcpclient.ServerSpec{ID: "github", Type: mcpclient.TypeStdio, Command: []string{"/bin/github"}},
				From: mcpclient.ScopeProject, FromID: "project:p1", EntryID: "github"},
		},
		SelectedIDs: []string{"github"},
	}}
	b := New(quietLogger())
	b.SetScopeResolver(resolver)

	// BRAINSTORM: may not act ⇒ the operator's server is NOT offered.
	sb := newAskSession(b, "cb", filepath.Join(t.TempDir(), "ask"))
	sb.tenantID = "tnt_dev"
	sb.projectID = "p1"
	sb.mode = askmode.Brainstorm
	cfg := mcpConfigOf(t, sb.argv())
	if strings.Contains(cfg, `"github"`) {
		t.Errorf("brainstorm was OFFERED the operator's MCP server: %s", cfg)
	}
	if !strings.Contains(cfg, `"orchicon"`) {
		t.Errorf("the platform's own sidecar must stay offered in every mode: %s", cfg)
	}

	// ITERATION: may act ⇒ the operator's server IS offered.
	si := newAskSession(b, "ci", filepath.Join(t.TempDir(), "ask"))
	si.tenantID = "tnt_dev"
	si.projectID = "p1"
	si.mode = askmode.Iteration
	cfg = mcpConfigOf(t, si.argv())
	if !strings.Contains(cfg, `"github"`) {
		t.Errorf("iteration was NOT offered the operator's MCP server: %s", cfg)
	}
}

// mcpConfigOf returns the --mcp-config value from an argv, failing if absent.
func mcpConfigOf(t *testing.T, argv []string) string {
	t.Helper()
	for i, a := range argv {
		if a == "--mcp-config" && i+1 < len(argv) {
			return argv[i+1]
		}
	}
	t.Fatal("the Ask argv has no --mcp-config")
	return ""
}
