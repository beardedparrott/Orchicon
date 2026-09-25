package askorchicon

// consent.go — the consent core for an Ask Orchicon turn.
//
// opencode raises a `permission.asked` bus event for every write/edit/bash a
// turn performs under the interactive profile. Before this file, the drain
// loop answered every ask with a hardcoded "once" (the `--auto` equivalent) and
// threw the event's detail away, so there was nothing to show a user and no way
// to answer "once for this directory". This file supplies the three missing
// pieces:
//
//   - a TYPED extraction of the ask (tool + target paths / command) from the
//     raw opencode property vocabulary (extractAskAction);
//   - the DECISION PATH below the never-allow binary class, delegated to
//     internal/permpolicy.Store.Decide (never restated here);
//   - a pending-ask registry (awaiting a human reply) and an in-memory,
//     directory-keyed, per-conversation session GRANT store.
//
// SETTLED DECISION (do not "fix" this into a second source of truth): the value
// we POST to the serve is ALWAYS `once` or `reject`. The SESSION decision
// ("allow for this directory") lives in OUR grant store, keyed by the target's
// directory and scoped to the conversation. That keeps the grant state
// authoritative in one place, lets the persistent deny/accept policy be
// consulted first, and is robust across serve builds that may not support a
// session-scoped response value.

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/neverallow"
	"github.com/beardedparrott/orchicon/internal/permpolicy"
	"github.com/beardedparrott/orchicon/internal/scheduler"
)

// ---------------------------------------------------------------------------
// Typed detail extraction
// ---------------------------------------------------------------------------

// askAction is the TYPED detail extracted from one permission.asked event.
type askAction struct {
	// Tool is the opencode permission name ("write" | "edit" | "batch_write" |
	// "bash" | ...), as opencode spells it.
	Tool string
	// Targets are the paths a write/edit/batch_write touches, as the model
	// wrote them (relative targets stay relative — resolveAskKey anchors them).
	Targets []string
	// Command is the bash command line, verbatim, for a shell ask.
	Command string
	// CallID is the tool call the ask belongs to (opencode emits it alongside
	// the ask id); advisory, used for transcript correlation.
	CallID string
	// Key is the grant/deny KEY (C4): a cleaned directory. For a file action it
	// is the target's directory; for bash it is the cwd's directory. It is set
	// by resolveAskKey (the extraction is scope-blind; the scope is not known
	// until the decision).
	Key string
	// Input is the correlated tool-call ARGS of the call this ask gates (the
	// adapter attaches them as Detail["toolInput"], see
	// internal/opencode toolCallIndex). They are the ONLY detail an MCP /
	// host-suite ask has: opencode gates such a call by its tool key and emits
	// `patterns: ["*"], metadata: {}`. Nil when the call was not observed.
	Input map[string]any
}

// askActionResolved reports whether the ask carries something to KEY a
// decision on: real target path(s) or a command. An ask that resolves to
// neither is kept deliberately distinct from one that resolves to the scope
// directory — treating the two the same is what let a sibling-path write
// through an `orchicon_*` tool be approved silently (see decide's fail-closed
// arm).
func askActionResolved(a askAction) bool {
	return len(realTargets(a.Targets)) > 0 || strings.TrimSpace(a.Command) != ""
}

// isWildcardPattern reports whether a permission pattern is opencode's
// "everything" rule rather than a path — `*`, `**`, `*/*`, `/**`. Such an entry
// is the permission RULE an MCP-tool ask carries, never a target: keying it as
// one resolves to <scope>/*, which looks like the scope directory and therefore
// like "inside the project".
func isWildcardPattern(s string) bool {
	star := false
	for _, r := range s {
		switch r {
		case '*':
			star = true
		case '/', '\\', ' ', '\t', '.':
			// path separators, spaces and a bare '.' are neutral
		default:
			return false
		}
	}
	return star
}

// realTargets drops wildcard-only entries from an ask's target list.
func realTargets(in []string) []string {
	out := make([]string, 0, len(in))
	for _, t := range in {
		t = strings.TrimSpace(t)
		if t == "" || t == "." || isWildcardPattern(t) {
			continue
		}
		out = append(out, t)
	}
	return out
}

// anyList normalises a decoded JSON array (either []any or a typed slice) to
// []any for iteration.
func anyList(v any) []any {
	switch t := v.(type) {
	case []any:
		return t
	case []map[string]any:
		out := make([]any, 0, len(t))
		for _, m := range t {
			out = append(out, m)
		}
		return out
	}
	return nil
}

// toolInputTargets pulls the paths out of a correlated tool call's args. The
// host suite's mutating tools spell them `filePath` (write/edit) and `path`
// (batch_write's per-write entries, `paths` for a list); other MCP tools use
// their own vocabulary, so the candidates stay defensive — the same shape the
// built-in ask's `metadata.filepath` uses.
func toolInputTargets(in map[string]any) []string {
	if len(in) == 0 {
		return nil
	}
	if t := realTargets(detailStrings(in, "filePath", "filepath", "file_path", "path", "target", "file")); len(t) > 0 {
		return t
	}
	if t := realTargets(detailStrings(in, "paths", "targets", "files")); len(t) > 0 {
		return t
	}
	// batch_write: {"writes": [{"path": ..., "mode": ...}, ...]}
	var out []string
	for _, e := range anyList(in["writes"]) {
		if m, ok := e.(map[string]any); ok {
			out = append(out, realTargets(detailStrings(m, "path", "filePath", "filepath"))...)
		}
	}
	return out
}

// toolInputCommand pulls a shell command line out of a correlated tool call's
// args (`bash` spells it `command`).
func toolInputCommand(in map[string]any) string {
	return detailString(in, "command", "cmd", "script", "shellCommand")
}

// compactArgs renders a correlated call's args for a card when NO target or
// command could be recognised, so the user sees what the tool will do rather
// than only its name. Values are bounded (a write's `content` can be huge).
func compactArgs(in map[string]any) string {
	keys := make([]string, 0, len(in))
	for k := range in {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for i, k := range keys {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(k)
		b.WriteString("=")
		s := fmt.Sprintf("%v", in[k])
		if len(s) > 80 {
			s = s[:80] + "…"
		}
		b.WriteString(s)
	}
	return b.String()
}

// isBashAsk reports whether a tool name is a shell-command ask. A shell command
// has no target path — it is NOT path-scopable (`curl`, `systemctl status`,
// `ps aux` all have no target directory), so it is keyed on the cwd and shown
// as a command.
func isBashAsk(tool string) bool {
	t := strings.ToLower(strings.TrimSpace(tool))
	return t == "bash" || t == "shell" || t == "sh" || strings.Contains(t, "bash")
}

// detailString returns the first non-empty string among the candidate keys.
func detailString(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k].(string); ok && strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// detailStrings returns the first candidate key that yields a non-empty list of
// strings, either as a []string or a []any of strings.
func detailStrings(m map[string]any, keys ...string) []string {
	for _, k := range keys {
		switch v := m[k].(type) {
		case []string:
			if len(v) > 0 {
				return v
			}
		case []any:
			out := make([]string, 0, len(v))
			for _, e := range v {
				if s, ok := e.(string); ok && strings.TrimSpace(s) != "" {
					out = append(out, s)
				}
			}
			if len(out) > 0 {
				return out
			}
		case string:
			if strings.TrimSpace(v) != "" {
				return []string{v}
			}
		}
	}
	return nil
}

// extractAskAction turns a permission event's raw properties into an askAction.
//
// This is the ONLY place the opencode property vocabulary is interpreted, so a
// serve build that spells a field differently is fixed in one place. The
// candidate keys are DEFENSIVE (C7) because the tree's only fixture
// (chat_session_test.go busPermissionAsked) is our own belief; the captured
// golden payload (internal/opencode/testdata/permission_asked_golden.json) is
// the authority — if the live shape disagrees, the golden file wins and the
// candidates below are corrected.
//
// Pure: no ctx, no db.
func extractAskAction(evt scheduler.SessionEvent) askAction {
	d := evt.Detail
	if d == nil {
		return askAction{Key: ""}
	}
	a := askAction{
		Tool:   detailString(d, "permission", "tool", "title"),
		CallID: detailString(d, "callID", "callId", "call_id"),
	}
	// The REAL schema nests the tool call under `tool` ({messageID, callID}) and
	// has no top-level callID, so the flat candidates above never match a live
	// payload. Kept for older/other shapes; the nested read is the real one.
	if a.CallID == "" {
		if tl, ok := d["tool"].(map[string]any); ok {
			a.CallID = detailString(tl, "callID", "callId", "call_id")
		}
	}
	a.Targets = realTargets(detailStrings(d, "patterns", "pattern"))
	meta, _ := d["metadata"].(map[string]any)
	if meta != nil {
		if len(a.Targets) == 0 {
			if p := detailString(meta, "filePath", "filepath", "path", "file"); p != "" {
				a.Targets = realTargets([]string{p})
			}
		}
		a.Command = detailString(meta, "command")
	}
	// The correlated tool-call args (see internal/opencode toolCallIndex) are
	// the ONLY detail an MCP / host-suite ask has: such an ask ships
	// `patterns: ["*"]` and `metadata: {}`, so without this the action has no
	// target and no command — the shape that used to be judged "inside the
	// project" and silently approved.
	if in, ok := d["toolInput"].(map[string]any); ok && len(in) > 0 {
		a.Input = in
		if len(a.Targets) == 0 {
			a.Targets = toolInputTargets(in)
		}
		if a.Command == "" {
			a.Command = toolInputCommand(in)
		}
	}
	if isBashAsk(a.Tool) {
		if a.Command == "" && len(a.Targets) > 0 {
			a.Command = a.Targets[0]
		}
		// A shell command is not a path: the command IS the detail, so the
		// "patterns" list must not masquerade as target paths on the card.
		a.Targets = nil
	}
	return a
}

// binaryClassRefusal returns a refusal string when a bash action's command is a
// member of the never-allow binary class (C6). The class is checked ABOVE
// permpolicy.Decide on purpose: its own doc says it stops below the class, and
// the class must NEVER prompt, so it is refused before a card is ever emitted.
// Empty for a non-bash action or a command the class does not name.
func binaryClassRefusal(a askAction) string {
	if !isBashAsk(a.Tool) || strings.TrimSpace(a.Command) == "" {
		return ""
	}
	cmd := strings.TrimSpace(a.Command)
	for _, pat := range neverallow.CommandPatterns {
		if shellGlobMatch(pat, cmd) {
			return "never allow: this command is in the never-allow class (pattern " + strconvQuote(pat) + ") and can never be approved, not even with a directory grant"
		}
	}
	return ""
}

// shellGlobMatch matches an opencode permission pattern against a command
// string. The class's patterns are SHELL globs: `*` deliberately crosses
// everything, including `/` and shell operators (`* && sudo *` must match a
// smuggled invocation), which doublestar's path semantics would refuse. Only
// `*` and `?` are significant; every other character is literal.
func shellGlobMatch(pat, s string) bool {
	var b strings.Builder
	b.WriteString("^")
	for _, r := range pat {
		switch r {
		case '*':
			b.WriteString(".*")
		case '?':
			b.WriteString(".")
		default:
			b.WriteString(regexp.QuoteMeta(string(r)))
		}
	}
	b.WriteString("$")
	re, err := regexp.Compile(b.String())
	if err != nil {
		return false
	}
	return re.MatchString(s)
}

// strconvQuote quotes a pattern for a human-readable refusal.
func strconvQuote(s string) string { return "\"" + s + "\"" }

// resolveAskKey computes the grant/deny KEY (C4/C5) for an action against the
// resolved scope directory:
//
//   - a file write/edit keys on the TARGET's directory (Dir of the absolute
//     target); a multi-target batch_write keys on the FIRST target's directory
//     (documented decision: a second directory in the batch asks again, rather
//     than computing a common ancestor that would over-grant).
//   - bash keys on the cwd's directory (scope.Dir): a session grant for that
//     directory is the blanket escape for commands run there.
func resolveAskKey(a askAction, dir string) string {
	abs := absAskTarget(a, dir)
	if abs == "" {
		return ""
	}
	if isBashAsk(a.Tool) || len(a.Targets) == 0 {
		return filepath.Clean(dir)
	}
	return filepath.Dir(abs)
}

// absAskTarget returns the absolute path the policy is consulted on: the first
// target for a file action (resolved against the scope dir when relative), or
// the scope directory itself for a bash action.
func absAskTarget(a askAction, dir string) string {
	if len(a.Targets) == 0 || isBashAsk(a.Tool) {
		return filepath.Clean(dir)
	}
	t := filepath.Clean(a.Targets[0])
	if !filepath.IsAbs(t) {
		t = filepath.Join(filepath.Clean(dir), t)
	}
	return t
}

// askSummary is the one-line card text: the tool plus its target (or command),
// never just an opaque id.
func askSummary(a askAction) string {
	switch {
	case isBashAsk(a.Tool) && a.Command != "":
		return "run shell command: " + a.Command
	case len(a.Targets) > 0:
		return a.Tool + " " + strings.Join(a.Targets, ", ")
	case a.Command != "":
		return a.Tool + " " + a.Command
	case a.Tool != "" && len(a.Input) > 0:
		// No recognised target or command, but the call's args are known:
		// show them rather than a bare tool name.
		return a.Tool + " " + compactArgs(a.Input)
	case a.Tool != "":
		return a.Tool
	default:
		return "a tool permission request"
	}
}

// ---------------------------------------------------------------------------
// The grant store (in-memory, per conversation, directory-keyed)
// ---------------------------------------------------------------------------

// grantStore records session grants: "allow for this directory" answers. It is
// deliberately IN MEMORY with no persistence (C9): a plane restart clears it,
// and a new conversation asks again. Keyed conversation id -> cleaned directory.
type grantStore struct {
	mu     sync.Mutex
	byConv map[string]map[string]bool
}

func newGrantStore() *grantStore {
	return &grantStore{byConv: make(map[string]map[string]bool)}
}

// Grant records a session grant for dir under convID.
func (g *grantStore) Grant(convID, dir string) {
	if g == nil || convID == "" || strings.TrimSpace(dir) == "" {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	set := g.byConv[convID]
	if set == nil {
		set = make(map[string]bool)
		g.byConv[convID] = set
	}
	set[filepath.Clean(dir)] = true
}

// Has reports whether convID holds a grant for dir.
func (g *grantStore) Has(convID, dir string) bool {
	if g == nil || convID == "" || strings.TrimSpace(dir) == "" {
		return false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.byConv[convID][filepath.Clean(dir)]
}

// ClearConversation drops every grant for a conversation (conversation end).
func (g *grantStore) ClearConversation(convID string) {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.byConv, convID)
}

// Len reports how many grants a conversation holds (the AC assertion hook).
func (g *grantStore) Len(convID string) int {
	if g == nil {
		return 0
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.byConv[convID])
}

// ---------------------------------------------------------------------------
// The pending-ask registry
// ---------------------------------------------------------------------------

// askState is the lifecycle of one pending ask.
type askState int

const (
	// askOpen — awaiting a human reply.
	askOpen askState = iota
	// askClientReplied — the client answered; the turn applies it.
	askClientReplied
	// askFinalized — the turn ended and the ask was answered `reject`.
	askFinalized
)

// pendingAsk is one recorded, awaiting ask. The turn is NOT blocked by us:
// opencode holds the tool call, we record and await a reply.
type pendingAsk struct {
	AskID          string
	ConversationID string
	SessionID      string
	Action         askAction
	// Key is the directory grant/deny key (C4) the decision used.
	Key string
	// Tool / Command / Targets / Directory / InsideProject / Summary are the
	// CARD fields carried to the client (never just an opaque id).
	Tool          string
	Command       string
	Targets       []string
	Directory     string
	InsideProject bool
	Summary       string
	// reply wakes the drain loop's select when a decision lands.
	reply chan struct{}

	mu     sync.Mutex
	state  askState
	choice apiv1.PermissionChoice
}

// clientReply records a client decision. False when the ask is no longer open
// (already answered or finalized) — the caller reports it expired, never a
// silent success.
func (a *pendingAsk) clientReply(c apiv1.PermissionChoice) bool {
	a.mu.Lock()
	if a.state != askOpen {
		a.mu.Unlock()
		return false
	}
	a.state = askClientReplied
	a.choice = c
	a.mu.Unlock()
	// Buffered(1) wake-up: a reply that arrives while the drain loop is between
	// selects is never dropped.
	select {
	case a.reply <- struct{}{}:
	default:
	}
	return true
}

// clientChoice reports the client's recorded choice, if one landed.
func (a *pendingAsk) clientChoice() (apiv1.PermissionChoice, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.state == askClientReplied {
		return a.choice, true
	}
	return apiv1.PermissionChoice_CHOICE_UNSPECIFIED, false
}

// resolveForFinalize is the turn-end read of an ask: it returns the client's
// choice when one landed (applyClient=true), claims a still-open ask for
// expiry (wasOpen=true), and reports nothing for an ask already finalized.
func (a *pendingAsk) resolveForFinalize() (choice apiv1.PermissionChoice, applyClient, wasOpen bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	switch a.state {
	case askClientReplied:
		return a.choice, true, false
	case askOpen:
		a.state = askFinalized
		return apiv1.PermissionChoice_CHOICE_UNSPECIFIED, false, true
	default:
		return apiv1.PermissionChoice_CHOICE_UNSPECIFIED, false, false
	}
}

// pendingAskRegistry is keyed (conversation, ask id) — the work item's
// (conversation, session, permission id), with the session pinned on the entry
// (it can change across an attempt's session recreation).
type pendingAskRegistry struct {
	mu     sync.Mutex
	byConv map[string]map[string]*pendingAsk
}

func newPendingAskRegistry() *pendingAskRegistry {
	return &pendingAskRegistry{byConv: make(map[string]map[string]*pendingAsk)}
}

func (r *pendingAskRegistry) put(convID string, a *pendingAsk) {
	if r == nil || convID == "" || a == nil || a.AskID == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	set := r.byConv[convID]
	if set == nil {
		set = make(map[string]*pendingAsk)
		r.byConv[convID] = set
	}
	set[a.AskID] = a
}

func (r *pendingAskRegistry) get(convID, askID string) (*pendingAsk, bool) {
	if r == nil {
		return nil, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	a, ok := r.byConv[convID][askID]
	return a, ok
}

func (r *pendingAskRegistry) remove(convID, askID string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.byConv[convID], askID)
	if len(r.byConv[convID]) == 0 {
		delete(r.byConv, convID)
	}
}

// removeConversation drops every ask for a conversation and returns them, so
// the caller can expire (reject) each held tool call.
func (r *pendingAskRegistry) removeConversation(convID string) []*pendingAsk {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	set := r.byConv[convID]
	out := make([]*pendingAsk, 0, len(set))
	for _, a := range set {
		out = append(out, a)
	}
	delete(r.byConv, convID)
	return out
}

// list returns the conversation's open asks (order not guaranteed).
func (r *pendingAskRegistry) list(convID string) []*pendingAsk {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	set := r.byConv[convID]
	out := make([]*pendingAsk, 0, len(set))
	for _, a := range set {
		out = append(out, a)
	}
	return out
}

// ---------------------------------------------------------------------------
// The per-turn consent handle
// ---------------------------------------------------------------------------

// consentResponse maps a client choice to the value we POST to the serve.
// ALWAYS `once` or `reject` — the SESSION decision lives in our grant store
// (the settled C1 decision; asserted by the tests so a future reader does not
// "fix" this into a second source of truth).
func consentResponse(c apiv1.PermissionChoice) (string, bool) {
	switch c {
	case apiv1.PermissionChoice_ALLOW_ONCE, apiv1.PermissionChoice_ALLOW_SESSION:
		return "once", true
	case apiv1.PermissionChoice_DENY:
		return "reject", true
	default:
		return "", false
	}
}

// consentTurn is the per-turn handle the drain loop owns. One decision path,
// one wake-up channel; the turn is never blocked by us.
type consentTurn struct {
	svc      *Service
	convID   string
	tenantID string
	monitor  *chatStallMonitor
	ledger   *toolLedger

	// replies wakes the drain loop when a client decision lands.
	replies chan struct{}

	scopeOnce sync.Once
	scope     AskFileScope
}

func newConsentTurn(svc *Service, convID, tenantID string, monitor *chatStallMonitor, ledger *toolLedger) *consentTurn {
	return &consentTurn{
		svc:      svc,
		convID:   convID,
		tenantID: tenantID,
		monitor:  monitor,
		ledger:   ledger,
		replies:  make(chan struct{}, 1),
	}
}

// Reply is the drain loop's wake-up channel: a value arrives when a client
// decision has been recorded for one of this turn's asks.
func (ct *consentTurn) Reply() <-chan struct{} { return ct.replies }

// wake nudges the drain loop (best-effort, never blocks).
func (ct *consentTurn) wake() {
	select {
	case ct.replies <- struct{}{}:
	default:
	}
}

// scopeFor resolves (once per turn) the conversation's file/shell boundary. A
// resolution failure degrades to the zero scope — FromConversation=false, so
// nothing is pre-approved and the turn asks — matching AskFileScope's own
// "failing closed here means an unapproved directory asks".
func (ct *consentTurn) scopeFor(ctx context.Context) AskFileScope {
	ct.scopeOnce.Do(func() {
		s, err := AskFileScopeFor(ctx, ct.svc.pool)
		if err != nil {
			ct.log().Warn("ask orchicon consent: file scope unresolved — treating every ask as outside the project",
				"conversation", ct.convID, "error", err)
			return
		}
		ct.scope = s
	})
	return ct.scope
}

// log is the turn's logger, tolerating a Service built without one (bare
// unit tests).
func (ct *consentTurn) log() *slog.Logger {
	if ct.svc != nil && ct.svc.log != nil {
		return ct.svc.log
	}
	return slog.Default()
}

// record writes one consent decision into the turn's transcript ledger.
func (ct *consentTurn) record(a askAction, verdict, detail string) {
	target := a.Command
	if target == "" && len(a.Targets) > 0 {
		target = strings.Join(a.Targets, ", ")
	}
	ct.ledger.recordPermission(a.Tool, target, verdict, detail)
}

// decide is THE decision path (the whole precedence chain, in order):
//
//	never-allow binary class  -> refuse (never prompts; a grant cannot help)
//	permpolicy.Decide         -> deny | grant | accept | project | ask
//
// A non-empty response ("once" | "reject") means "answer the serve now with
// this value". An empty response with a non-nil ask means "await the human".
func (ct *consentTurn) decide(ctx context.Context, sid string, evt scheduler.SessionEvent) (response string, ask *pendingAsk, refusal string) {
	a := extractAskAction(evt)
	if r := binaryClassRefusal(a); r != "" {
		ct.record(a, "never_allow", r)
		return "reject", nil, r
	}
	scope := ct.scopeFor(ctx)
	a.Key = resolveAskKey(a, scope.Dir)
	abs := absAskTarget(a, scope.Dir)

	// FAIL CLOSED. An ask whose detail resolves to no target and no command
	// (an MCP/host-suite ask shipped with `patterns: ["*"]` and `metadata: {}`
	// — from a serve whose tool-part correlation is unavailable, or a shape we
	// do not recognise) must NOT ride the project/accept/grant verdicts on the
	// scope directory: absAskTarget degrades to the scope dir, which is
	// pre-approved, so "proceed" would approve a write whose path we never saw
	// (the QA finding: a sibling-path write through an `orchicon_*` tool raised
	// no ask). It is raised as a card instead, and the deny list still outranks
	// it below.
	resolved := askActionResolved(a)

	pol := askPermissionPolicy()
	d, err := pol.Decide(abs, permpolicy.Inputs{
		SessionGranted: ct.svc.grants.Has(ct.convID, a.Key),
		ProjectDefault: scope.PreApprovedPath(abs),
	})
	if err != nil {
		// Fail closed: a malformed policy file must never silently proceed.
		ct.record(a, "policy_error", err.Error())
		return "reject", nil, err.Error()
	}
	switch {
	case d.Verdict == permpolicy.VerdictDeny:
		ref := pol.Refusal(abs, d.Entry).Error()
		ct.record(a, "deny", ref)
		return "reject", nil, ref
	case d.Verdict.Proceed() && resolved:
		// grant | accept | project — proceed silently.
		ct.record(a, d.Verdict.String(), "")
		return "once", nil, ""
	}
	if !resolved {
		ct.log().Warn("ask orchicon consent: the ask carries no target path or command — asking rather than proceeding",
			"conversation", ct.convID, "ask", evt.PermissionID, "tool", a.Tool)
	}

	// VerdictAsk (or an unresolved action): record, emit the card, await the
	// human.
	summary := askSummary(a)
	if !resolved {
		summary += " (no path or command in the ask detail — asking rather than proceeding)"
	}
	ask = &pendingAsk{
		AskID:          evt.PermissionID,
		ConversationID: ct.convID,
		SessionID:      sid,
		Action:         a,
		Key:            a.Key,
		Tool:           a.Tool,
		Command:        a.Command,
		Targets:        a.Targets,
		Directory:      a.Key,
		// An unresolved action has no known target, so it cannot claim to be
		// inside the project (the card says why it is being asked instead).
		InsideProject: resolved && scope.PreApprovedPath(abs),
		Summary:       summary,
		reply:         ct.replies,
	}
	ct.svc.pending.put(ct.convID, ask)
	if ct.monitor != nil {
		ct.monitor.setAwaitingConsent(true)
	}
	ct.record(a, "ask", summary)
	return "", ask, ""
}

// applyClientReplies answers the serve for every ask whose client decision has
// landed. Called from the drain loop's reply arm.
func (ct *consentTurn) applyClientReplies(ctx context.Context, client scheduler.ChatTurnClient) {
	for _, a := range ct.svc.pending.list(ct.convID) {
		choice, ok := a.clientChoice()
		if !ok {
			continue
		}
		resp, ok := consentResponse(choice)
		if !ok {
			continue
		}
		if err := client.ReplyPermissionDecision(ctx, a.SessionID, a.AskID, resp); err != nil {
			ct.log().Warn("ask orchicon consent: reply to serve failed",
				"conversation", ct.convID, "ask", a.AskID, "response", resp, "error", err)
		}
		ct.record(a.Action, "user_"+choice.String(), "")
		ct.svc.pending.remove(ct.convID, a.AskID)
	}
	ct.settleMonitor()
}

// finalize ends the turn's consent state (C8): a client decision that landed
// but was not yet applied IS applied; every still-open ask is answered
// `reject` so the serve's session is not left holding a phantom permission;
// a late client reply then reports expired (never silence).
func (ct *consentTurn) finalize(ctx context.Context, client scheduler.ChatTurnClient) {
	for _, a := range ct.svc.pending.removeConversation(ct.convID) {
		choice, applyClient, wasOpen := a.resolveForFinalize()
		switch {
		case applyClient:
			resp, ok := consentResponse(choice)
			if !ok {
				continue
			}
			if err := client.ReplyPermissionDecision(ctx, a.SessionID, a.AskID, resp); err != nil {
				ct.log().Warn("ask orchicon consent: post-turn reply failed",
					"conversation", ct.convID, "ask", a.AskID, "error", err)
			}
			ct.record(a.Action, "user_"+choice.String(), "")
		case wasOpen:
			if err := client.ReplyPermissionDecision(ctx, a.SessionID, a.AskID, "reject"); err != nil {
				ct.log().Warn("ask orchicon consent: expiring unanswered ask failed",
					"conversation", ct.convID, "ask", a.AskID, "error", err)
			}
			ct.record(a.Action, "expired", "unanswered at turn end")
		}
	}
	ct.settleMonitor()
}

// settleMonitor clears the stall-monitor gate when no ask of this turn is open.
func (ct *consentTurn) settleMonitor() {
	if ct.monitor == nil {
		return
	}
	open := false
	for _, a := range ct.svc.pending.list(ct.convID) {
		a.mu.Lock()
		if a.state == askOpen {
			open = true
		}
		a.mu.Unlock()
		if open {
			break
		}
	}
	ct.monitor.setAwaitingConsent(open)
}

// emitPermissionAsk carries the ask to the client on the turn's stream. Because
// onStreamEvent fans into the hub (chat.go), a re-attached watcher receives it
// for free.
func emitPermissionAsk(emit func(*apiv1.ChatStreamResponse), a *pendingAsk) {
	if emit == nil || a == nil {
		return
	}
	emit(&apiv1.ChatStreamResponse{
		Event: &apiv1.ChatStreamResponse_PermissionAsk{
			PermissionAsk: &apiv1.PermissionAsk{
				AskId:          a.AskID,
				ConversationId: a.ConversationID,
				SessionId:      a.SessionID,
				Tool:           a.Tool,
				Command:        a.Command,
				Targets:        a.Targets,
				Directory:      a.Directory,
				InsideProject:  a.InsideProject,
				Summary:        a.Summary,
			},
		},
	})
}

// logAsk is a small diagnostic used by the drain loops when a card is emitted.
func (ct *consentTurn) logAsk(a *pendingAsk) {
	ct.log().Info("ask orchicon consent: awaiting a decision",
		"conversation", ct.convID, "ask", a.AskID, "tool", a.Tool,
		"directory", a.Directory, "summary", a.Summary)
}
