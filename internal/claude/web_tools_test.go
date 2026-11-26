package claude

// web_tools_test.go — WebSearch/WebFetch must raise a CARD in Ask, not be silently denied.
//
// THE BUG THESE PIN. Neither name was in AskPermissionToolNames, which drives BOTH `permissions.ask`
// and the `PermissionRequest` matcher, and neither was in HookToolMatcher, so no hook ran for them
// either. Both tools then fell through to claude's own permission flow, which in a `-p` session has
// nobody to grant, and every call came back:
//
//	"Claude requested permissions to use WebSearch, but you haven't granted it yet."
//
// with nothing in the GUI or TUI the operator could approve. The mechanics are the same ones the
// `mcp__.*` matcher fix documents (permissions.go, HookToolMatcher): a tool the matcher omits never
// reaches the hook, and a tool the Ask profile does not prompt on never reaches a human.

import (
	"context"
	"regexp"
	"strings"
	"testing"
	"time"
)

// webTools are the two tools this fix is about, and the assertion set is built from ONE list so a
// third web tool added later has an obvious place to land.
var webTools = []string{"WebSearch", "WebFetch"}

// permissionRequestMatcher reads the Ask profile's `PermissionRequest` matcher out of the document the
// CLI is actually handed. A missing hook means no card at all, so it is a fatal, not an error.
func permissionRequestMatcher(t *testing.T) string {
	t.Helper()
	doc := settingsDoc(t, launchArgs(t, ProfileAskEnvValue))
	hooks, ok := doc["hooks"].(map[string]any)
	if !ok {
		t.Fatal("the Ask profile's settings carry no hooks block")
	}
	entries, ok := hooks["PermissionRequest"].([]any)
	if !ok || len(entries) == 0 {
		t.Fatal("the Ask profile registers NO PermissionRequest hook — the permission system would ask " +
			"and nothing would answer, which is the terminal denial the operator sees as " +
			"\"you haven't granted it yet\"")
	}
	entry, ok := entries[0].(map[string]any)
	if !ok {
		t.Fatalf("the PermissionRequest entry is not an object: %T", entries[0])
	}
	m, _ := entry["matcher"].(string)
	if strings.TrimSpace(m) == "" {
		t.Fatal("the PermissionRequest entry names no matcher, so no tool is covered")
	}
	return m
}

// preToolUseMatcher reads the PreToolUse matcher for a profile. It is the hook's only entry point.
func preToolUseMatcher(t *testing.T, profile string) string {
	t.Helper()
	doc := settingsDoc(t, launchArgs(t, profile))
	hooks, ok := doc["hooks"].(map[string]any)
	if !ok {
		t.Fatalf("the %s profile's settings carry no hooks block", profile)
	}
	entries, ok := hooks["PreToolUse"].([]any)
	if !ok || len(entries) == 0 {
		t.Fatalf("the %s profile registers no PreToolUse hook", profile)
	}
	entry, ok := entries[0].(map[string]any)
	if !ok {
		t.Fatalf("the %s profile's PreToolUse entry is not an object: %T", profile, entries[0])
	}
	m, _ := entry["matcher"].(string)
	if strings.TrimSpace(m) == "" {
		t.Fatalf("the %s profile's PreToolUse entry names no matcher", profile)
	}
	return m
}

// matcherMatches compiles a matcher the way claude does — as a whole-name regular expression.
func matcherMatches(t *testing.T, matcher, tool string) bool {
	t.Helper()
	re, err := regexp.Compile("^(?:" + matcher + ")$")
	if err != nil {
		t.Fatalf("the matcher %q is not a valid regex: %v", matcher, err)
	}
	return re.MatchString(tool)
}

// THE ASK PROFILE MUST PROMPT ON THE WEB TOOLS — this is the whole fix, asserted on the document the
// CLI is handed rather than on the constants, so a change that compiles but never reaches the CLI
// still fails here.
func TestWebToolsAskProfileSettings(t *testing.T) {
	ask := askList(t, ProfileAskEnvValue)
	if len(ask) == 0 {
		t.Fatal("the Ask profile's settings carry NO permissions.ask list, so nothing raises a card")
	}
	prMatcher := permissionRequestMatcher(t)
	for _, tool := range webTools {
		if !containsString(ask, tool) {
			t.Errorf("permissions.ask does not name %q (got %v) — the tool would fall through to claude's "+
				"own flow, which in -p mode is the silent \"you haven't granted it yet\" denial with no card",
				tool, ask)
		}
		if !matcherMatches(t, prMatcher, tool) {
			t.Errorf("the PermissionRequest matcher %q does not match %q — the hook would never fire, so the "+
				"ask the permission system raises would have nobody to answer it", prMatcher, tool)
		}
	}
	// AND THE PRETOOLUSE MATCHER COVERS THEM TOO, so the hook's own verdict (policy, never-allow,
	// protected path) applies on the way in rather than being skipped.
	pre := preToolUseMatcher(t, ProfileAskEnvValue)
	for _, tool := range webTools {
		if !matcherMatches(t, pre, tool) {
			t.Errorf("the PreToolUse matcher %q does not match %q — the hook never sees the call, so its "+
				"policy and never-allow verdicts cannot apply", pre, tool)
		}
	}
}

// THE WORKER PROFILE GETS NO ASK ENTRY FOR THEM. A headless worker has nobody to answer a card, and an
// unanswered ask is a REFUSAL — adding the web tools to the worker's ask list would turn a working
// fetch into a permission error.
func TestWebToolsWorkerProfileNotInAsk(t *testing.T) {
	if got := askList(t, ProfileWorkerEnvValue); len(got) != 0 {
		t.Fatalf("the WORKER profile carries permissions.ask %v — a headless worker has nobody to answer, "+
			"so every ask resolves as a refusal", got)
	}
	// The worker's hook STILL sees them, so the operator's policy deny and the never-allow class apply.
	pre := preToolUseMatcher(t, ProfileWorkerEnvValue)
	for _, tool := range webTools {
		if !matcherMatches(t, pre, tool) {
			t.Errorf("the worker PreToolUse matcher %q does not match %q — the hook would never run, which "+
				"is exactly the silent-denial shape this fix removes", pre, tool)
		}
	}
	// AND THE WORKER RULE SET ALLOWS THEM: parity with the native bridge, which has no web gate at all.
	// The containment is the operator's policy, not a blanket deny.
	for _, tool := range webTools {
		v := DecideTool(HookInput{ToolName: tool, ToolInput: map[string]any{"url": "https://example.com"}}, "/tmp/proj", "")
		if !v.Allow {
			t.Errorf("DecideTool(%s) refuses in the worker profile (rule %q, reason %q) — a non-interactive "+
				"worker cannot answer a card, so a deny here is a permanently broken web tool", tool, v.Rule, v.Reason)
		}
	}
}

// THE ASK RULE SET DEFERS THEM TO THE OPERATOR rather than deciding for them: an Ask conversation must
// get a card, and it must get one from the PermissionRequest hook (a PreToolUse ask is a tool ERROR,
// not a card — consent_hook.go).
func TestWebToolsAskVerdict(t *testing.T) {
	for _, tool := range webTools {
		v := DecideToolForAsk(HookInput{ToolName: tool, ToolInput: map[string]any{"url": "https://example.com"}}, "/tmp/ask", "")
		if d := v.Decision(); d != DecisionAsk {
			t.Errorf("DecideToolForAsk(%s) = %s, want %s — an interactive session must be asked, not decided "+
				"for", tool, d, DecisionAsk)
		}
	}
}

// THE CARD ITSELF: a WebFetch ask over the consent socket must reach the operator with the URL in it.
//
// The bus event is what BOTH clients render (the TUI and the GUI raise a card for any `permission`
// event), so an event whose Target list is empty is a card that asks the operator to approve nothing.
func TestWebFetchConsentCardCarriesURL(t *testing.T) {
	const url = "https://antigravity.google/docs"
	b, s, bus := consentHarness(t)

	done := make(chan hookConsentReply, 1)
	go func() {
		done <- askOverSocket(t, s.consentSockPath(), "WebFetch", map[string]any{"url": url})
	}()

	ev := nextEvent(t, bus)
	if ev.Kind != "permission" {
		t.Fatalf("the adapter emitted %q, want a permission event — without it no card is raised at all", ev.Kind)
	}
	if ev.Tool != "WebFetch" {
		t.Errorf("the ask names tool %q, want WebFetch", ev.Tool)
	}
	if !containsString(ev.Targets, url) {
		t.Errorf("the WebFetch card carries targets %v, which do not include the URL %q — the operator would "+
			"be asked to approve a fetch of an unnamed page", ev.Targets, url)
	}
	if ev.PermissionID == "" {
		t.Fatal("the ask carries no id, so the operator's answer can never be correlated to it")
	}

	// APPROVING IT LETS THE CALL RUN: the hook must be told allow, or the card is decorative.
	if err := b.ReplyPermissionDecision(context.Background(), s.sid, ev.PermissionID, "once"); err != nil {
		t.Fatalf("ReplyPermissionDecision: %v", err)
	}
	select {
	case got := <-done:
		if got.Behavior != "allow" {
			t.Fatalf("the hook was told %q, want allow — the operator approved and the fetch would still be refused", got.Behavior)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the hook never got an answer — it would sit until the CLI killed it while the operator believed they had approved")
	}
}

// A REFUSED WebFetch MUST CARRY THE MODEL-FACING REFUSAL TEXT, not a bare error: that text is what
// tells the model the call did not run and not to retry (consent_hook.go, the deny branch).
func TestWebFetchConsentCardRefusalExplainsItself(t *testing.T) {
	const url = "https://antigravity.google/docs"
	b, s, bus := consentHarness(t)

	done := make(chan hookConsentReply, 1)
	go func() {
		done <- askOverSocket(t, s.consentSockPath(), "WebFetch", map[string]any{"url": url})
	}()

	ev := nextEvent(t, bus)
	if err := b.ReplyPermissionDecision(context.Background(), s.sid, ev.PermissionID, "reject"); err != nil {
		t.Fatalf("ReplyPermissionDecision: %v", err)
	}
	select {
	case got := <-done:
		if got.Behavior != "deny" {
			t.Fatalf("the hook was told %q, want deny", got.Behavior)
		}
		if !strings.Contains(got.Message, "refused") {
			t.Errorf("the refusal message is %q — it must tell the model the call did not run and not to retry", got.Message)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the hook never got the refusal")
	}
}

// AND A WebSearch's QUERY IS ITS TARGET — the search has no URL, so the query is the only honest
// description of what is being consented to.
func TestWebSearchConsentCardCarriesQuery(t *testing.T) {
	const query = "antigravity orchicon docs"
	b, s, bus := consentHarness(t)

	done := make(chan hookConsentReply, 1)
	go func() {
		done <- askOverSocket(t, s.consentSockPath(), "WebSearch", map[string]any{"query": query})
	}()

	ev := nextEvent(t, bus)
	if ev.Kind != "permission" {
		t.Fatalf("the adapter emitted %q, want a permission event", ev.Kind)
	}
	if ev.Tool != "WebSearch" {
		t.Errorf("the ask names tool %q, want WebSearch", ev.Tool)
	}
	if !containsString(ev.Targets, query) {
		t.Errorf("the WebSearch card carries targets %v, which do not include the query %q — the operator "+
			"would approve a search they cannot see", ev.Targets, query)
	}
	if err := b.ReplyPermissionDecision(context.Background(), s.sid, ev.PermissionID, "once"); err != nil {
		t.Fatalf("ReplyPermissionDecision: %v", err)
	}
	select {
	case got := <-done:
		if got.Behavior != "allow" {
			t.Fatalf("the hook was told %q, want allow", got.Behavior)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the hook never got an answer")
	}
}

// THE WHOLE PATH THROUGH THE REAL HOOK ENTRY POINT: `RunHook` with HookEventName=PermissionRequest is
// what the CLI actually executes (`orchicon claude-hook`), so this checks the wire the operator's
// approval travels on, not just the socket in isolation.
func TestWebFetchPermissionRequestHookEndToEnd(t *testing.T) {
	const url = "https://antigravity.google/docs"
	_, s, bus := consentHarness(t)

	env := map[string]string{
		HookProfileEnv: ProfileAskEnvValue,
		ConsentSockEnv: s.consentSockPath(),
	}
	done := make(chan string, 1)
	go func() {
		var out strings.Builder
		RunHook(strings.NewReader(`{"hook_event_name":"PermissionRequest","tool_name":"WebFetch","tool_input":{"url":"`+url+`"}}`),
			&out, func(k string) string { return env[k] })
		done <- out.String()
	}()

	ev := nextEvent(t, bus)
	if ev.Kind != "permission" || ev.Tool != "WebFetch" {
		t.Fatalf("the CLI hook produced event kind=%q tool=%q, want a permission event naming WebFetch", ev.Kind, ev.Tool)
	}
	if !containsString(ev.Targets, url) {
		t.Errorf("the card raised by the real hook carries targets %v without the URL %q", ev.Targets, url)
	}
	if err := s.b.ReplyPermissionDecision(context.Background(), s.sid, ev.PermissionID, "once"); err != nil {
		t.Fatalf("ReplyPermissionDecision: %v", err)
	}
	select {
	case wire := <-done:
		if !strings.Contains(wire, `"hookEventName":"PermissionRequest"`) {
			t.Errorf("the hook wrote %q, want a PermissionRequest document", wire)
		}
		if !strings.Contains(wire, `"behavior":"allow"`) {
			t.Fatalf("the hook wrote %q, want a PermissionRequest allow — this exact shape was measured to run the call", wire)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the hook never returned — the CLI would kill it and the fetch would be denied with no card")
	}
}
