package acp

// replay_test.go — ONE TEST PER RECORDED FIXTURE, plus the guard that keeps the list complete.
//
// Each test drives the real client (Start, Initialize, Authenticate, sessions, prompts) against a
// fake process replaying child 1's recording in lockstep, then asserts the decoded update sequence,
// the turn result and anything the fixture specifically demonstrates.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/beardedparrott/orchicon/internal/adapter"
)

// ctxT is a test context with a generous deadline.
func ctxT(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// TestReplay_CoversEveryFixture fails when the directory and the fixture list disagree, so a new
// recording cannot be silently left unreplayed.
func TestReplay_CoversEveryFixture(t *testing.T) {
	matches, err := filepath.Glob(filepath.Join(fixtureDir, "*.jsonl"))
	if err != nil {
		t.Fatalf("glob fixtures: %v", err)
	}
	var onDisk []string
	for _, m := range matches {
		name := strings.TrimSuffix(filepath.Base(m), ".jsonl")
		if name+".jsonl" == offwireFixture {
			continue
		}
		onDisk = append(onDisk, name)
	}
	sort.Strings(onDisk)
	want := append([]string(nil), fixtures...)
	sort.Strings(want)
	if strings.Join(onDisk, ",") != strings.Join(want, ",") {
		t.Fatalf("fixture coverage mismatch:\n  on disk: %v\n  tested:  %v", onDisk, want)
	}
	if len(want) != 14 {
		t.Fatalf("replayable fixture count = %d, want 14", len(want))
	}
	// The off-wire probe transcript must be present but unreplayed: it has no protocol lines.
	if _, err := os.Stat(filepath.Join(fixtureDir, offwireFixture)); err != nil {
		t.Fatalf("off-wire fixture missing: %v", err)
	}
	if n := fixtureC2SCount(loadFixture(t, offwireFixture)); n != 0 {
		t.Fatalf("%s has %d c2s lines; it must not be replayable", offwireFixture, n)
	}
}

// TestReplay_SimpleTurn: the minimal happy-path turn. Asserted in full — the update sequence, the
// stop reason and the per-turn usage — because it is the shape both bridges rely on.
func TestReplay_SimpleTurn(t *testing.T) {
	ctx := ctxT(t)
	c, fp := newTestClient(t, allowAllHandler{})
	done := runReplay(fp, loadFixture(t, "simple_turn.jsonl"))

	if _, err := c.Initialize(ctx); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if err := c.Authenticate(ctx, "oauth-personal"); err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	sess, state, err := c.NewSession(ctx, NewSessionParams{Cwd: "/tmp/orchicon/acp/simple_turn"})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	if got, want := len(state.Models), 27; got != want {
		t.Errorf("availableModels = %d, want %d", got, want)
	}
	if state.CurrentModelID != "gemini-4-neon-high" {
		t.Errorf("currentModelId = %q, want gemini-4-neon-high", state.CurrentModelID)
	}
	sink := newEventSink(sess)
	if err := sess.SetModel(ctx, "gemini-3.8-flash-low"); err != nil {
		t.Fatalf("SetModel: %v", err)
	}
	res, err := sess.Prompt(ctx, []ContentBlock{TextBlock("Reply with the single word OK. Do not use any tools.")})
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	awaitReplay(t, done)

	if res.StopReason != StopReasonEndTurn {
		t.Errorf("stopReason = %q, want %q", res.StopReason, StopReasonEndTurn)
	}
	if res.Usage.TotalTokens != 9065 {
		t.Errorf("usage.totalTokens = %d, want 9065", res.Usage.TotalTokens)
	}
	sink.waitStable(t, 7, 5*time.Second)
	mustKindSequence(t, sink.kinds(),
		KindConfigOptionUpdate, KindAvailableCommands, KindUsageUpdate,
		KindAvailableCommands, KindSessionInfoUpdate, KindAgentMessageChunk, KindUsageUpdate)

	msg := sink.firstOfKind(t, KindAgentMessageChunk, time.Second).(AgentMessageChunk)
	if msg.Content.Text != "OK" {
		t.Errorf("agent message = %q, want OK", msg.Content.Text)
	}
	if msg.SessionID() != sess.ID() {
		t.Errorf("update sessionId = %q, want %q", msg.SessionID(), sess.ID())
	}
	if usage, ok := sink.firstOfKind(t, KindUsageUpdate, time.Second).(UsageUpdate); ok {
		if usage.Size != 1048576 {
			t.Errorf("usage window size = %d, want 1048576", usage.Size)
		}
	}
	// Every update must be a modelled kind: an Unknown here would mean the client lost data.
	for _, u := range sink.all() {
		if _, bad := u.(Unknown); bad {
			t.Errorf("unexpected Unknown update %q in a recorded turn", u.SessionUpdate())
		}
	}
}

// TestReplay_AskQuestion: ask_question is NOT a client-side request — it arrives as a tool_call
// with kind "other" and its questions in rawInput. This pins that, so nobody adds an ask handler.
func TestReplay_AskQuestion(t *testing.T) {
	ctx := ctxT(t)
	c, fp := newTestClient(t, allowAllHandler{})
	done := runReplay(fp, loadFixture(t, "ask_question.jsonl"))

	if _, err := c.Initialize(ctx); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if err := c.Authenticate(ctx, "oauth-personal"); err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	sess, _, err := c.NewSession(ctx, NewSessionParams{Cwd: "/tmp/orchicon/acp/ask_question"})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	sink := newEventSink(sess)
	if err := sess.SetModel(ctx, "gemini-3.8-flash-low"); err != nil {
		t.Fatalf("SetModel: %v", err)
	}
	if _, err := sess.Prompt(ctx, []ContentBlock{TextBlock("choose red or blue")}); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	awaitReplay(t, done)

	sink.waitStable(t, 9, 5*time.Second)
	mustKindSequence(t, sink.kinds(),
		KindConfigOptionUpdate, KindAvailableCommands, KindAvailableCommands, KindUsageUpdate,
		KindSessionInfoUpdate, KindUsageUpdate, KindToolCall, KindToolCallUpdate, KindAgentMessageChunk)

	call, ok := sink.firstOfKind(t, KindToolCall, time.Second).(ToolCall)
	if !ok {
		t.Fatalf("tool_call did not decode as ToolCall")
	}
	if call.Name != "ask_question" {
		t.Errorf("tool name = %q, want ask_question", call.Name)
	}
	if call.Kind != "other" {
		t.Errorf("tool kind = %q, want other", call.Kind)
	}
	// rawInput.questions[0].options is how the question surfaces; decode it through the tool.
	var raw struct {
		Questions []struct {
			Question string   `json:"question"`
			Options  []string `json:"options"`
		} `json:"questions"`
	}
	if err := json.Unmarshal(call.RawInput, &raw); err != nil {
		t.Fatalf("decode rawInput: %v", err)
	}
	if len(raw.Questions) != 1 || len(raw.Questions[0].Options) != 2 {
		t.Fatalf("rawInput.questions = %+v, want one question with two options", raw.Questions)
	}
	if raw.Questions[0].Options[0] != "(Recommended) Red" {
		t.Errorf("first option = %q, want \"(Recommended) Red\"", raw.Questions[0].Options[0])
	}
}

// TestReplay_AuthRequired: both auth failures map onto *AuthError, and the gemini-api-key text —
// which matches none of the shared signals — still produces the actionable instruction.
func TestReplay_AuthRequired(t *testing.T) {
	ctx := ctxT(t)
	c, fp := newTestClient(t, allowAllHandler{})
	done := runReplay(fp, loadFixture(t, "auth_required.jsonl"))

	if _, err := c.Initialize(ctx); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	// session/new succeeds BEFORE auth was ever attempted: agy defers the credential check.
	sess, _, err := c.NewSession(ctx, NewSessionParams{Cwd: "/tmp/orchicon/acp/signedout"})
	if err != nil {
		t.Fatalf("NewSession before authenticate: %v", err)
	}

	err = c.Authenticate(ctx, "no-such-auth-method")
	if err == nil {
		t.Fatalf("Authenticate(no-such-auth-method) succeeded, want an auth error")
	}
	var authErr *AuthError
	if !asAuthError(err, &authErr) {
		t.Fatalf("Authenticate(no-such-auth-method) = %T %v, want *AuthError", err, err)
	}
	if !strings.Contains(authErr.Detail, "Unsupported auth method") {
		t.Errorf("AuthError.Detail = %q, want it to carry agy's own text", authErr.Detail)
	}

	err = c.Authenticate(ctx, "gemini-api-key")
	if err == nil {
		t.Fatalf("Authenticate(gemini-api-key) succeeded, want an auth error")
	}
	if !asAuthError(err, &authErr) {
		t.Fatalf("Authenticate(gemini-api-key) = %T %v, want *AuthError", err, err)
	}
	// The critical assertion: agy's "GEMINI_API_KEY is not set" matches no shared signal, so only
	// the blanket mapping in Client.Authenticate can produce the operator instruction.
	if authErr.Message != adapter.AntigravityAuthRequiredMessage {
		t.Errorf("AuthError.Message is not the actionable instruction:\n got: %q\nwant: %q",
			authErr.Message, adapter.AntigravityAuthRequiredMessage)
	}
	if !strings.Contains(authErr.Error(), "GEMINI_API_KEY is not set") {
		t.Errorf("AuthError.Error() lost agy's text: %q", authErr.Error())
	}

	sink := newEventSink(sess)
	if err := sess.SetModel(ctx, "gemini-3.8-flash-low"); err != nil {
		t.Fatalf("SetModel: %v", err)
	}
	res, err := sess.Prompt(ctx, []ContentBlock{TextBlock("Reply with the single word OK.")})
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	awaitReplay(t, done)

	// agy 1.5.13 still answers the turn (it falls back internally); the point here is that the
	// client did not mistake an unauthenticated session for a signed-in one.
	if res.StopReason != StopReasonEndTurn {
		t.Errorf("stopReason = %q, want end_turn", res.StopReason)
	}
	sink.waitStable(t, 7, 5*time.Second)
	mustKindSequence(t, sink.kinds(),
		KindAvailableCommands, KindUsageUpdate, KindConfigOptionUpdate, KindAvailableCommands,
		KindSessionInfoUpdate, KindAgentMessageChunk, KindUsageUpdate)
}

// asAuthError is errors.As without the import churn in every call site.
func asAuthError(err error, target **AuthError) bool {
	ae, ok := err.(*AuthError)
	if ok {
		*target = ae
	}
	return ok
}

// TestReplay_Compact: a second prompt on the same session after /compact — the session stays
// usable and each turn terminates on its own result.
func TestReplay_Compact(t *testing.T) {
	ctx := ctxT(t)
	c, fp := newTestClient(t, allowAllHandler{})
	done := runReplay(fp, loadFixture(t, "compact.jsonl"))

	if _, err := c.Initialize(ctx); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if err := c.Authenticate(ctx, "oauth-personal"); err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	sess, _, err := c.NewSession(ctx, NewSessionParams{Cwd: "/tmp/orchicon/acp/compact"})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	sink := newEventSink(sess)
	if err := sess.SetModel(ctx, "gemini-3.8-flash-low"); err != nil {
		t.Fatalf("SetModel: %v", err)
	}
	first, err := sess.Prompt(ctx, []ContentBlock{TextBlock("Reply with the single word READY.")})
	if err != nil {
		t.Fatalf("first Prompt: %v", err)
	}
	if first.Usage.TotalTokens != 9064 {
		t.Errorf("first turn totalTokens = %d, want 9064", first.Usage.TotalTokens)
	}
	second, err := sess.Prompt(ctx, []ContentBlock{TextBlock("/compact")})
	if err != nil {
		t.Fatalf("second Prompt: %v", err)
	}
	awaitReplay(t, done)

	if second.StopReason != StopReasonEndTurn {
		t.Errorf("second stopReason = %q, want end_turn", second.StopReason)
	}
	if second.Usage.TotalTokens != 10191 {
		t.Errorf("compacted turn totalTokens = %d, want 10191", second.Usage.TotalTokens)
	}
	sink.waitStable(t, 8, 5*time.Second)
	mustKindSequence(t, sink.kinds(),
		KindConfigOptionUpdate, KindAvailableCommands, KindAvailableCommands, KindUsageUpdate,
		KindSessionInfoUpdate, KindAgentMessageChunk, KindUsageUpdate, KindUsageUpdate)
}

// TestReplay_FileEditDiff: tool_call_update must decode its `content[]` diff items, which is where
// a file edit's actual text lives.
func TestReplay_FileEditDiff(t *testing.T) {
	ctx := ctxT(t)
	c, fp := newTestClient(t, allowAllHandler{})
	done := runReplay(fp, loadFixture(t, "file_edit_diff.jsonl"))

	if _, err := c.Initialize(ctx); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if err := c.Authenticate(ctx, "oauth-personal"); err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	sess, _, err := c.NewSession(ctx, NewSessionParams{Cwd: "/tmp/orchicon/acp/file_edit_diff"})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	sink := newEventSink(sess)
	if err := sess.SetModel(ctx, "gemini-3.8-flash-low"); err != nil {
		t.Fatalf("SetModel: %v", err)
	}
	if _, err := sess.Prompt(ctx, []ContentBlock{TextBlock("create and modify a.txt")}); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	awaitReplay(t, done)

	sink.waitStable(t, 18, 5*time.Second)
	mustKindSequence(t, sink.kinds(),
		KindConfigOptionUpdate, KindAvailableCommands, KindAvailableCommands, KindUsageUpdate,
		KindSessionInfoUpdate, KindUsageUpdate,
		KindToolCall, KindToolCallUpdate, KindUsageUpdate,
		KindToolCall, KindToolCallUpdate, KindUsageUpdate,
		KindToolCall, KindToolCallUpdate,
		KindAgentMessageChunk, KindAgentMessageChunk, KindAgentMessageChunk, KindUsageUpdate)

	var diffs []ToolContent
	for _, u := range sink.all() {
		tu, ok := u.(ToolCallUpdate)
		if !ok {
			continue
		}
		for _, c := range tu.Content {
			if c.Type == "diff" {
				diffs = append(diffs, c)
			}
		}
	}
	if len(diffs) < 2 {
		t.Fatalf("diff content items = %d, want at least 2 (write_file then edit_file)", len(diffs))
	}
	if diffs[0].NewText != "x" {
		t.Errorf("first diff newText = %q, want \"x\"", diffs[0].NewText)
	}
	if !strings.HasSuffix(diffs[0].Path, "a.txt") {
		t.Errorf("first diff path = %q, want it to end with a.txt", diffs[0].Path)
	}
	if diffs[1].OldText != "x" || diffs[1].NewText != "y" {
		t.Errorf("second diff = %q -> %q, want \"x\" -> \"y\"", diffs[1].OldText, diffs[1].NewText)
	}
	if len(diffs[0].Raw) == 0 {
		t.Error("ToolContent.Raw is empty; the raw form must be retained")
	}
}

// TestReplay_HandshakeAuth: capabilities are recorded, and the systemPrompt append is emitted as
// the _meta.antigravity.systemPrompt.append STRING (agy ignores a non-string there).
func TestReplay_HandshakeAuth(t *testing.T) {
	ctx := ctxT(t)
	c, fp := newTestClient(t, allowAllHandler{})
	done := runReplay(fp, loadFixture(t, "handshake_auth.jsonl"))

	init, err := c.Initialize(ctx)
	if err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if !init.AgentCapabilities.LoadSession {
		t.Error("agentCapabilities.loadSession = false, want true")
	}
	if !init.AgentCapabilities.PromptCapabilities.EmbeddedContext {
		t.Error("promptCapabilities.embeddedContext = false, want true")
	}
	if !init.AgentCapabilities.SessionCapability("resume") {
		t.Error("sessionCapabilities.resume missing")
	}
	if got, want := len(init.AuthMethods), 4; got != want {
		t.Errorf("authMethods = %d, want %d", got, want)
	}
	if c.Capabilities().LoadSession != true {
		t.Error("Capabilities() did not retain the recorded handshake")
	}

	if err := c.Authenticate(ctx, "oauth-personal"); err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	sess, _, err := c.NewSession(ctx, NewSessionParams{
		Cwd:                "/tmp/orchicon/acp/handshake_auth",
		SystemPromptAppend: "You are recorded by the Orchicon spike recorder. Ignore this line.",
	})
	if err != nil {
		t.Fatalf("NewSession with systemPrompt append: %v", err)
	}
	sink := newEventSink(sess)
	if err := sess.SetModel(ctx, "gemini-3.8-flash-low"); err != nil {
		t.Fatalf("SetModel: %v", err)
	}
	// The second session/new is the fixture's non-string append probe: this client never emits a
	// non-string, so it sends no _meta at all and agy's quirk cannot be triggered from here.
	if _, _, err := c.NewSession(ctx, NewSessionParams{Cwd: "/tmp/orchicon/acp/handshake_auth"}); err != nil {
		t.Fatalf("second NewSession: %v", err)
	}
	awaitReplay(t, done)

	// The wire check the fixture exists for: the append is a string under _meta.antigravity.
	var sawAppend bool
	for _, doc := range fp.writtenDocs() {
		var d c2sDoc
		if err := json.Unmarshal(doc, &d); err != nil {
			t.Fatalf("decode written doc: %v", err)
		}
		if d.Method != methodSessionNew {
			continue
		}
		var p struct {
			Meta struct {
				Antigravity struct {
					SystemPrompt struct {
						Append json.RawMessage `json:"append"`
					} `json:"systemPrompt"`
				} `json:"antigravity"`
			} `json:"_meta"`
		}
		if err := json.Unmarshal(d.Params, &p); err != nil {
			t.Fatalf("decode session/new params: %v", err)
		}
		if len(p.Meta.Antigravity.SystemPrompt.Append) == 0 {
			continue
		}
		sawAppend = true
		if p.Meta.Antigravity.SystemPrompt.Append[0] != '"' {
			t.Errorf("systemPrompt.append was sent as %s, want a JSON string",
				p.Meta.Antigravity.SystemPrompt.Append)
		}
	}
	if !sawAppend {
		t.Error("no session/new carried _meta.antigravity.systemPrompt.append")
	}
	// The second session/new's updates belong to ITS session, not this sink, so this session sees
	// only its own four pre-turn updates.
	sink.waitStable(t, 4, 5*time.Second)
	mustKindSequence(t, sink.kinds(),
		KindConfigOptionUpdate, KindAvailableCommands, KindAvailableCommands, KindUsageUpdate)
}

// TestReplay_LoadSession: session/list, session/load (which replays history as notifications
// BEFORE its response) and session/resume (which replays nothing).
func TestReplay_LoadSession(t *testing.T) {
	ctx := ctxT(t)
	c, fp := newTestClient(t, allowAllHandler{})
	done := runReplay(fp, loadFixture(t, "load_session.jsonl"))
	const sid = "e71c8e2b-b1b9-46af-aba3-bbbf4e59159e"

	if _, err := c.Initialize(ctx); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if err := c.Authenticate(ctx, "oauth-personal"); err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	sessions, err := c.ListSessions(ctx)
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(sessions) != 11 {
		t.Errorf("session/list returned %d sessions, want 11", len(sessions))
	}

	sess, state, err := c.LoadSession(ctx, sid, LoadParams{Cwd: "/tmp/orchicon/acp/simple_turn"})
	if err != nil {
		t.Fatalf("LoadSession: %v", err)
	}
	if state.SessionID != sid {
		t.Errorf("loaded session id = %q, want %q", state.SessionID, sid)
	}
	sink := newEventSink(sess)
	// The replay arrives BEFORE the load response, so the events must already be queued by the
	// time LoadSession returned. That is what the pre-registration in attachSession buys.
	sink.waitForCount(t, KindUsageUpdate, 1, 5*time.Second)
	replayKinds := sink.kinds()
	wantReplay := []string{KindUserMessageChunk, KindAgentMessageChunk, KindSessionInfoUpdate, KindUsageUpdate}
	if len(replayKinds) < len(wantReplay) {
		t.Fatalf("replayed kinds = %v, want them to start with %v", replayKinds, wantReplay)
	}
	for i, want := range wantReplay {
		if replayKinds[i] != want {
			t.Fatalf("replayed kinds = %v, want them to start with %v", replayKinds, wantReplay)
		}
	}
	user := sink.firstOfKind(t, KindUserMessageChunk, time.Second).(UserMessageChunk)
	if !strings.Contains(user.Content.Text, "Reply with the single word OK") {
		t.Errorf("replayed user chunk = %q, want the original prompt", user.Content.Text)
	}

	if _, _, err := c.ResumeSession(ctx, sid, LoadParams{Cwd: "/tmp/orchicon/acp/simple_turn"}); err != nil {
		t.Fatalf("ResumeSession: %v", err)
	}
	if err := sess.SetModel(ctx, "gemini-4-neon-low"); err != nil {
		t.Fatalf("SetModel: %v", err)
	}
	if err := sess.SetModel(ctx, "claude-sonnet-5-5-low"); err != nil {
		t.Fatalf("second SetModel: %v", err)
	}
	if err := sess.SetMode(ctx, "accept-edits"); err != nil {
		t.Fatalf("SetMode: %v", err)
	}
	awaitReplay(t, done)

	sink.waitStable(t, 10, 5*time.Second)
	mustKindSequence(t, sink.kinds(),
		KindUserMessageChunk, KindAgentMessageChunk, KindSessionInfoUpdate, KindUsageUpdate,
		KindAvailableCommands, KindConfigOptionUpdate, KindConfigOptionUpdate,
		KindCurrentModeUpdate, KindConfigOptionUpdate, KindSessionInfoUpdate)
	if mode, ok := sink.firstOfKind(t, KindCurrentModeUpdate, time.Second).(CurrentModeUpdate); ok {
		if mode.ModeID != "accept-edits" {
			t.Errorf("current mode = %q, want accept-edits", mode.ModeID)
		}
	}
}

// TestReplay_McpToolCall: a tool call from an MCP server, gated by a permission request the
// handler allows.
func TestReplay_McpToolCall(t *testing.T) {
	ctx := ctxT(t)
	saw := make(chan PermissionRequest, 4)
	c, fp := newTestClient(t, allowAllHandler{saw: saw})
	done := runReplay(fp, loadFixture(t, "mcp_tool_call.jsonl"))

	if _, err := c.Initialize(ctx); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if err := c.Authenticate(ctx, "oauth-personal"); err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	sess, _, err := c.NewSession(ctx, NewSessionParams{
		Cwd: "/tmp/orchicon/acp/mcp_tool_call",
		MCPServers: []MCPServer{{
			Name:    "echo",
			Command: "python3",
			Args:    []string{"<repo>/internal/antigravity/testdata/acp/mcp_echo_server.py"},
		}},
	})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	sink := newEventSink(sess)
	if err := sess.SetModel(ctx, "gemini-3.8-flash-low"); err != nil {
		t.Fatalf("SetModel: %v", err)
	}
	if _, err := sess.Prompt(ctx, []ContentBlock{TextBlock("call the echo tool")}); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	awaitReplay(t, done)

	select {
	case req := <-saw:
		if req.ToolCall.Name != "mcp_echo_echo" {
			t.Errorf("permission tool name = %q, want mcp_echo_echo", req.ToolCall.Name)
		}
		if len(req.Options) != 2 {
			t.Errorf("permission options = %d, want 2", len(req.Options))
		}
		if req.Raw == nil {
			t.Error("PermissionRequest.Raw is empty; the raw params must be retained")
		}
	default:
		t.Fatal("the handler was never asked for permission")
	}
	sink.waitStable(t, 13, 5*time.Second)
	mustKindSequence(t, sink.kinds(),
		KindConfigOptionUpdate, KindAvailableCommands, KindAvailableCommands, KindUsageUpdate,
		KindSessionInfoUpdate, KindUsageUpdate,
		KindToolCall, KindToolCallUpdate, KindToolCallUpdate, KindToolCallUpdate,
		KindAgentMessageChunk, KindAgentMessageChunk, KindUsageUpdate)
}

// TestReplay_ThinkingTurn: reasoning tokens are visible on the RESULT, while no
// agent_thought_chunk update is emitted on the wire (docs 3.1). Asserting the absence is the
// point — it is why the thought-chunk type needs a synthetic test.
func TestReplay_ThinkingTurn(t *testing.T) {
	ctx := ctxT(t)
	c, fp := newTestClient(t, allowAllHandler{})
	done := runReplay(fp, loadFixture(t, "thinking_turn.jsonl"))

	if _, err := c.Initialize(ctx); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if err := c.Authenticate(ctx, "oauth-personal"); err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	sess, _, err := c.NewSession(ctx, NewSessionParams{Cwd: "/tmp/orchicon/acp/thinking_turn"})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	sink := newEventSink(sess)
	if err := sess.SetModel(ctx, "gemini-3.8-flash-high"); err != nil {
		t.Fatalf("SetModel: %v", err)
	}
	res, err := sess.Prompt(ctx, []ContentBlock{TextBlock("What is 17*23? Think step by step, then give the number.")})
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	awaitReplay(t, done)

	if res.Usage.ThoughtTokens != 237 {
		t.Errorf("usage.thoughtTokens = %d, want 237", res.Usage.ThoughtTokens)
	}
	if res.Usage.TotalTokens != 9513 {
		t.Errorf("usage.totalTokens = %d, want 9513", res.Usage.TotalTokens)
	}
	sink.waitStable(t, 14, 5*time.Second)
	if _, ok := sink.findKind(KindAgentThoughtChunk); ok {
		t.Fatal("agy emitted an agent_thought_chunk, contradicting docs 3.1 — update the doc and this test")
	}
}

// TestReplay_ToolPermissionReject: the handler denies, the client writes the recorded reject
// optionId, and the tool call ends failed.
func TestReplay_ToolPermissionReject(t *testing.T) {
	ctx := ctxT(t)
	saw := make(chan PermissionRequest, 4)
	c, fp := newTestClient(t, rejectHandler{saw: saw})
	done := runReplay(fp, loadFixture(t, "tool_permission_reject.jsonl"))

	if _, err := c.Initialize(ctx); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if err := c.Authenticate(ctx, "oauth-personal"); err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	sess, _, err := c.NewSession(ctx, NewSessionParams{Cwd: "/tmp/orchicon/acp/tool_permission_reject"})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	sink := newEventSink(sess)
	if err := sess.SetModel(ctx, "gemini-3.8-flash-low"); err != nil {
		t.Fatalf("SetModel: %v", err)
	}
	if _, err := sess.Prompt(ctx, []ContentBlock{TextBlock("run echo rejected")}); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	awaitReplay(t, done)

	select {
	case req := <-saw:
		if got, ok := RejectOptionID(req.Options); !ok || got != "deny" {
			t.Errorf("RejectOptionID = %q,%v, want \"deny\",true", got, ok)
		}
	default:
		t.Fatal("the handler was never asked for permission")
	}
	sink.waitStable(t, 10, 5*time.Second)
	mustKindSequence(t, sink.kinds(),
		KindConfigOptionUpdate, KindAvailableCommands, KindAvailableCommands, KindUsageUpdate,
		KindSessionInfoUpdate, KindUsageUpdate,
		KindToolCall, KindToolCallUpdate, KindToolCallUpdate, KindToolCallUpdate)
	if last := sink.all()[len(sink.all())-1].(ToolCallUpdate); last.Status != StatusFailed {
		t.Errorf("final tool status = %q, want failed", last.Status)
	}
	// assertC2S in the replay already compared this answer with the recording; state it plainly.
	if !answeredWith(t, fp, "deny") {
		t.Error("the client did not answer the permission request with the recorded reject option")
	}
}

// TestReplay_ToolReadFile: an ungated read tool with a session/set_mode before the prompt (not
// every fixture uses set_model).
func TestReplay_ToolReadFile(t *testing.T) {
	ctx := ctxT(t)
	c, fp := newTestClient(t, allowAllHandler{})
	done := runReplay(fp, loadFixture(t, "tool_read_file.jsonl"))

	if _, err := c.Initialize(ctx); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if err := c.Authenticate(ctx, "oauth-personal"); err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	sess, _, err := c.NewSession(ctx, NewSessionParams{Cwd: "/tmp/orchicon/acp/tool_read_file"})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	sink := newEventSink(sess)
	if err := sess.SetMode(ctx, "accept-edits"); err != nil {
		t.Fatalf("SetMode: %v", err)
	}
	if _, err := sess.Prompt(ctx, []ContentBlock{TextBlock("read hello.txt")}); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	awaitReplay(t, done)

	sink.waitStable(t, 11, 5*time.Second)
	mustKindSequence(t, sink.kinds(),
		KindCurrentModeUpdate, KindConfigOptionUpdate, KindAvailableCommands, KindUsageUpdate,
		KindSessionInfoUpdate, KindUsageUpdate, KindToolCall, KindToolCallUpdate,
		KindAgentMessageChunk, KindAgentMessageChunk, KindUsageUpdate)
	if call, ok := sink.firstOfKind(t, KindToolCall, time.Second).(ToolCall); ok {
		if call.Kind != "read" {
			t.Errorf("tool kind = %q, want read", call.Kind)
		}
		// read_file: the tool call itself already carries the location the update repeats.
		if len(call.Locations) != 1 || !strings.HasSuffix(call.Locations[0].Path, "hello.txt") {
			t.Errorf("tool_call locations = %v, want one path ending in hello.txt", call.Locations)
		}
	}
}

// TestReplay_ToolRunCommandPermissionAllow: the handler allows, the client writes the recorded
// allow optionId, and the command's output arrives as tool_call_update content.
func TestReplay_ToolRunCommandPermissionAllow(t *testing.T) {
	ctx := ctxT(t)
	saw := make(chan PermissionRequest, 4)
	c, fp := newTestClient(t, allowAllHandler{saw: saw})
	done := runReplay(fp, loadFixture(t, "tool_run_command_permission_allow.jsonl"))

	if _, err := c.Initialize(ctx); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if err := c.Authenticate(ctx, "oauth-personal"); err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	sess, _, err := c.NewSession(ctx, NewSessionParams{Cwd: "/tmp/orchicon/acp/tool_run_command_permission_allow"})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	sink := newEventSink(sess)
	if err := sess.SetModel(ctx, "gemini-3.8-flash-low"); err != nil {
		t.Fatalf("SetModel: %v", err)
	}
	if _, err := sess.Prompt(ctx, []ContentBlock{TextBlock("run echo hi")}); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	awaitReplay(t, done)

	select {
	case req := <-saw:
		if got, ok := AllowOptionID(req.Options); !ok || got != "allow" {
			t.Errorf("AllowOptionID = %q,%v, want \"allow\",true", got, ok)
		}
		if req.ToolCall.Kind != "execute" {
			t.Errorf("permission tool kind = %q, want execute", req.ToolCall.Kind)
		}
	default:
		t.Fatal("the handler was never asked for permission")
	}
	if !answeredWith(t, fp, "allow") {
		t.Error("the client did not answer the permission request with the recorded allow option")
	}

	sink.waitStable(t, 13, 5*time.Second)
	mustKindSequence(t, sink.kinds(),
		KindConfigOptionUpdate, KindAvailableCommands, KindUsageUpdate, KindAvailableCommands,
		KindSessionInfoUpdate, KindUsageUpdate,
		KindToolCall, KindToolCallUpdate, KindToolCallUpdate, KindToolCallUpdate,
		KindAgentMessageChunk, KindAgentMessageChunk, KindUsageUpdate)

	var output string
	for _, u := range sink.all() {
		tu, ok := u.(ToolCallUpdate)
		if !ok || tu.Status != StatusCompleted {
			continue
		}
		for _, c := range tu.Content {
			if c.Type == "content" {
				output += c.Text()
			}
		}
	}
	if !strings.Contains(output, "hi") {
		t.Errorf("completed tool output = %q, want it to contain hi", output)
	}
}

// TestReplay_UnknownModel: an unknown model id is a -32602 RPC error and leaves the session
// usable.
func TestReplay_UnknownModel(t *testing.T) {
	ctx := ctxT(t)
	c, fp := newTestClient(t, allowAllHandler{})
	done := runReplay(fp, loadFixture(t, "unknown_model.jsonl"))

	if _, err := c.Initialize(ctx); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if err := c.Authenticate(ctx, "oauth-personal"); err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	sess, _, err := c.NewSession(ctx, NewSessionParams{Cwd: "/tmp/orchicon/acp/unknown_model"})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	sink := newEventSink(sess)

	err = sess.SetModel(ctx, "nonexistent-x")
	if err == nil {
		t.Fatalf("SetModel(nonexistent-x) succeeded, want an RPC error")
	}
	var rpcErr *RPCError
	if !errorsAsRPC(err, &rpcErr) {
		t.Fatalf("SetModel error = %T %v, want *RPCError", err, err)
	}
	if rpcErr.Code != CodeInvalidParams {
		t.Errorf("error code = %d, want %d", rpcErr.Code, CodeInvalidParams)
	}
	if !strings.Contains(rpcErr.Message, "Unknown model ID") {
		t.Errorf("error message = %q, want agy's text", rpcErr.Message)
	}
	awaitReplay(t, done)

	// The session is still usable: the failing call did not poison the connection.
	sink.waitStable(t, 2, 5*time.Second)
	mustKindSequence(t, sink.kinds(), KindAvailableCommands, KindUsageUpdate)
	if c.Exited() {
		t.Fatal("the client exited after a benign RPC error")
	}
}

// errorsAsRPC is errors.As for *RPCError without an import at every call site.
func errorsAsRPC(err error, target **RPCError) bool {
	re, ok := err.(*RPCError)
	if ok {
		*target = re
	}
	return ok
}

// answeredWith reports whether the client wrote a permission answer selecting optionID.
func answeredWith(t *testing.T, fp *fakeProc, optionID string) bool {
	t.Helper()
	for _, doc := range fp.writtenDocs() {
		var d c2sDoc
		if err := json.Unmarshal(doc, &d); err != nil {
			continue
		}
		if d.Method != "" || len(d.Result) == 0 {
			continue
		}
		var outcome permissionOutcome
		if err := json.Unmarshal(d.Result, &outcome); err != nil {
			continue
		}
		if outcome.Outcome.Outcome == "selected" && outcome.Outcome.OptionID == optionID {
			return true
		}
	}
	return false
}

// TestReplay_CancelMidturn: cancellation ends the turn with stopReason "cancelled", and a
// permission request left outstanding at that moment is answered `{"outcome":"cancelled"}` — the
// same answer agy's own recording gives it.
//
// This one is hand-driven rather than replayed in full lockstep: this client answers the
// outstanding permission as soon as the cancel is sent, while the recording shows agy receiving
// that answer only after the turn ended. Both orders are valid; the assertion is on the ANSWER,
// not on where in the stream it appears.
func TestReplay_CancelMidturn(t *testing.T) {
	ctx := ctxT(t)
	handler := &blockingHandler{gate: make(chan struct{})}
	c, fp := newTestClient(t, handler)

	entries := loadFixture(t, "cancel_midturn.jsonl")
	// Drop the trailing agent-answer entry; this test asserts it from the written stream instead.
	trimmed := entries[:len(entries)-1]
	if last := entries[len(entries)-1]; last.dir != "c2s" {
		t.Fatalf("cancel_midturn last entry is %q, want the permission answer", last.dir)
	}
	done := runReplay(fp, trimmed)

	if _, err := c.Initialize(ctx); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if err := c.Authenticate(ctx, "oauth-personal"); err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	sess, _, err := c.NewSession(ctx, NewSessionParams{Cwd: "/tmp/orchicon/acp/cancel_midturn"})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	sink := newEventSink(sess)
	if err := sess.SetModel(ctx, "gemini-3.8-flash-low"); err != nil {
		t.Fatalf("SetModel: %v", err)
	}

	// Turn 1: stream, then cancel.
	firstDone := make(chan *PromptResult, 1)
	firstErr := make(chan error, 1)
	go func() {
		res, err := sess.Prompt(ctx, []ContentBlock{TextBlock("write 500 words about pencils")})
		firstDone <- res
		firstErr <- err
	}()
	sink.waitForCount(t, KindAgentMessageChunk, 2, 10*time.Second)
	if err := sess.Cancel(); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	select {
	case res := <-firstDone:
		if err := <-firstErr; err != nil {
			t.Fatalf("first Prompt: %v", err)
		}
		if res == nil || res.StopReason != StopReasonCancelled {
			t.Fatalf("first turn stopReason = %+v, want cancelled", res)
		}
		if res.Usage.TotalTokens != 9096 {
			t.Errorf("first turn totalTokens = %d, want 9096", res.Usage.TotalTokens)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("first Prompt did not return after cancel")
	}

	// Turn 2: cancel while a permission request is outstanding.
	secondDone := make(chan *PromptResult, 1)
	secondErr := make(chan error, 1)
	go func() {
		res, err := sess.Prompt(ctx, []ContentBlock{TextBlock("run echo still-here")})
		secondDone <- res
		secondErr <- err
	}()
	// The handler blocks, so this event only appears because the reader kept reading.
	handler.waitEntered(t, 10*time.Second)
	if err := sess.Cancel(); err != nil {
		t.Fatalf("Cancel (turn 2): %v", err)
	}
	select {
	case res := <-secondDone:
		if err := <-secondErr; err != nil {
			t.Fatalf("second Prompt: %v", err)
		}
		if res == nil || res.StopReason != StopReasonCancelled {
			t.Fatalf("second turn stopReason = %+v, want cancelled", res)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("second Prompt did not return after cancel with a pending permission")
	}
	awaitReplay(t, done)

	if !handler.wasCancelled() {
		t.Error("the blocked permission handler's context was not cancelled by the session cancel")
	}
	// The outstanding permission must be answered `cancelled` — never with an optionId.
	if !answeredCancelled(t, fp) {
		t.Fatalf("the outstanding permission was not answered cancelled; writes: %s",
			string(joinDocs(fp.writtenDocs())))
	}
	if answeredWith(t, fp, "allow") {
		t.Fatal("the client answered a permission request with allow, which it must never do unasked")
	}
}

// blockingHandler blocks every permission request until released or its context ends.
type blockingHandler struct {
	gate chan struct{}
	mu   sync.Mutex
	ctxs []context.Context
}

func (h *blockingHandler) RequestPermission(ctx context.Context, _ PermissionRequest) (PermissionDecision, error) {
	h.mu.Lock()
	h.ctxs = append(h.ctxs, ctx)
	h.mu.Unlock()
	select {
	case <-ctx.Done():
		return PermissionDecision{}, ctx.Err()
	case <-h.gate:
		return PermissionDecision{OptionID: "allow"}, nil
	}
}

func (h *blockingHandler) waitEntered(t *testing.T, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	for {
		h.mu.Lock()
		n := len(h.ctxs)
		h.mu.Unlock()
		if n > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the permission handler was never entered")
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func (h *blockingHandler) wasCancelled() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, ctx := range h.ctxs {
		select {
		case <-ctx.Done():
		default:
			return false
		}
	}
	return len(h.ctxs) > 0
}

// answeredCancelled reports whether the client wrote a `{"outcome":"cancelled"}` permission answer.
func answeredCancelled(t *testing.T, fp *fakeProc) bool {
	t.Helper()
	for _, doc := range fp.writtenDocs() {
		var d c2sDoc
		if err := json.Unmarshal(doc, &d); err != nil {
			continue
		}
		var outcome permissionOutcome
		if err := json.Unmarshal(d.Result, &outcome); err != nil {
			continue
		}
		if outcome.Outcome.Outcome == "cancelled" {
			return true
		}
	}
	return false
}

// joinDocs renders written documents for a failure message.
func joinDocs(docs []json.RawMessage) []byte {
	out := []byte("[")
	for i, d := range docs {
		if i > 0 {
			out = append(out, ", "...)
		}
		out = append(out, d...)
	}
	return append(out, ']')
}
