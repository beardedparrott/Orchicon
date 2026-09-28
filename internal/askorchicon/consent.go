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
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/guard"
	"github.com/beardedparrott/orchicon/internal/neverallow"
	"github.com/beardedparrott/orchicon/internal/permpolicy"
	"github.com/beardedparrott/orchicon/internal/protectedpath"
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
	// AskID is the ADAPTER's id for the consent ask this action gated (the
	// `permission.asked` id, or the native bridge's permission id).
	//
	// IT IS PERSISTED, and that is the whole reason it is here. The decision the
	// operator makes is written into the turn's ledger as a synthetic
	// `permission.<verdict>` record, which becomes part of the assistant message in
	// the database — so it is DURABLE, where the live stream event is not. Carrying
	// the id on that record is what lets ANY client reconcile its copy of a card
	// against server truth: a second tab, a device, or the same page after a
	// reload, none of which ever see the live event that settled it. Without the id
	// the record is a decision nobody can attach to the ask it decided, which is
	// exactly why this stayed broken through several attempts to fix it in the
	// stream.
	AskID string
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

// compactArgs renders a call's arguments for the card: SCALARS ONLY, and a
// shape (a count) for anything nested. It is the fallback for a card when NO
// target or command could be recognised, so the operator sees what the tool
// will do rather than only its name.
//
// It deliberately does NOT fall back to fmt's %v for a value it does not
// understand. That is exactly what put a Go struct literal on an operator's
// card — "writes=[map[mode:edit new:type chatBus struct {…]", truncated
// mid-struct at 80 chars — because a []map[string]any rendered through %v is
// Go's own debug formatting. An unrecognised shape is reported as an ellipsis:
// the card's job is to say what will happen, and a dump of internal structure
// says nothing while making the card unreadable.
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
		b.WriteString(scalarArg(in[k]))
	}
	return b.String()
}

// scalarArg renders one argument value. Scalars are shown as themselves,
// containers are summarised by SIZE, and anything else is an ellipsis — never a
// formatted dump of the value (see compactArgs).
func scalarArg(v any) string {
	switch t := v.(type) {
	case nil:
		return "null"
	case string:
		return truncateArg(t)
	case bool:
		return strconv.FormatBool(t)
	case float64, float32, int, int8, int16, int32, int64,
		uint, uint8, uint16, uint32, uint64, json.Number:
		return fmt.Sprintf("%v", t)
	case []any:
		return countLabel(len(t), "item")
	case map[string]any:
		return countLabel(len(t), "field")
	default:
		return "…"
	}
}

// countLabel renders a container's size: "[2 items]", "[1 field]".
func countLabel(n int, noun string) string {
	if n == 1 {
		return "[1 " + noun + "]"
	}
	return fmt.Sprintf("[%d %ss]", n, noun)
}

// truncateArg shortens a scalar so a long value cannot push the card's actions
// off the screen.
func truncateArg(s string) string {
	const max = 80
	if len(s) > max {
		return s[:max] + "…"
	}
	return s
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
	// TYPED CONSENT FIELDS FIRST (adapter-neutral).
	//
	// An adapter that can name the action directly — the native bridge knows the
	// tool, its command and its argument JSON at the moment it is about to run it
	// — must not have to shape them into opencode's property vocabulary. This is
	// the same rule the typed tool_result fields follow, and it is what lets a
	// second adapter raise a real ask without imitating the first.
	//
	// The Detail path below stays authoritative for opencode, which is the only
	// adapter that emits it.
	if evt.Tool != "" {
		a := askAction{
			Tool:    evt.Tool,
			Command: evt.Command,
			Targets: realTargets(evt.Targets),
		}
		if strings.TrimSpace(evt.InputJSON) != "" {
			var in map[string]any
			if err := json.Unmarshal([]byte(evt.InputJSON), &in); err == nil {
				a.Input = in
				// The arguments carry the target/command for a call whose own ask
				// named neither (an MCP/host-suite tool), so they are the fallback
				// rather than the override.
				if len(a.Targets) == 0 {
					a.Targets = toolInputTargets(in)
				}
				if a.Command == "" {
					a.Command = toolInputCommand(in)
				}
			}
		}
		return a
	}
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
	// The ask's own target(s). opencode's built-in write/edit ask carries them
	// TWICE: `patterns` is worktree-RELATIVE and `metadata.filepath` is
	// ABSOLUTE. The ABSOLUTE one wins — the Ask serve runs with NO
	// `--directory` (servehost.go), so its worktree base is the plane's cwd,
	// NOT the conversation's project dir; a relative pattern joined to the
	// project dir can therefore resolve INSIDE the project for a file opencode
	// itself placed outside, and be approved silently (AC1). `patterns` stays
	// the fallback for an ask that carries no path in its metadata.
	meta, _ := d["metadata"].(map[string]any)
	if meta != nil {
		if p := detailString(meta, "filePath", "filepath", "path", "file"); p != "" {
			a.Targets = realTargets([]string{p})
		}
		a.Command = detailString(meta, "command")
	}
	if len(a.Targets) == 0 {
		a.Targets = realTargets(detailStrings(d, "patterns", "pattern"))
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

// decisionTarget is one path the consent DECISION covers: the absolute path the
// policy is consulted on, plus the directory its grant/project inputs are read
// from.
type decisionTarget struct {
	abstarget string
	key       string
}

// (commandPaths lives below decisionTargets — see its own comment for what it does
// and does not extract.)

// absAskTarget returns the absolute path the policy is consulted on for the
// action's KEY target: the first target for a file action (resolved against
// the scope dir when relative), or the scope directory itself for a bash
// action. See decisionTargets for the paths the DECISION covers.
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

// commandPaths extracts the filesystem paths a shell command NAMES as arguments.
//
// DELIBERATELY MODEST, AND HONEST ABOUT IT. It finds LITERAL absolute paths — `/etc/x`,
// `--flag=/etc/x`, `~/x` — and nothing more. A path built at runtime (`$(cat cfg)`, a
// variable, a script's own logic) is invisible to it, so this RAISES the bar rather than
// guaranteeing containment, and the operator was told exactly that when they chose it. It
// is worth having because the case that prompted it (`cp ~/a /etc/b`) is a literal path in
// the text.
//
// WHAT IT MUST NOT DO IS INVENT PATHS, because an invented path is an ask no session grant
// can answer — and that is exactly what it did. The operator granted the project directory
// again and again and was asked again every time, because an awk program's REGEX was read
// as a path: `/^func` became a consent target whose grant key is
// filepath.Dir("/^func") = "/", which no grant can cover, so the ask repeated forever. The
// ledger of that session shows every `awk`/`sed` command asking, each answered with "never
// ask again", each asking again. Three rules keep invention out:
//
//  1. QUOTED TOKENS ARE DATA AND MUST LOOK LIKE PATHS. A quoted span is a program, a regex
//     or a literal, not an argument, so `'/^func /'` and `'/return x/'` are rejected on
//     SHAPE — two path segments or a home prefix, no metacharacter. Unquoted tokens keep
//     the original permissive rule: they are arguments, and `cp ~/a /etc/b` is the case this
//     function exists for.
//  2. A REGEX/GLOB METACHARACTER DISQUALIFIES A TOKEN outright, quoted or not. A token
//     containing any of `^ + ! * , ? [ ] { } ( ) |` or a backslash is a pattern, not a
//     target we could judge.
//  3. HEREDOC BODIES ARE FILE CONTENT, NOT ARGUMENTS. Everything between `<<'EOF'` and its
//     terminator is text about to be WRITTEN, so a path in it is not a path this command
//     touches — and reading them meant that writing a test file whose content mentioned
//     /tmp raised an ask about /tmp. The heredoc operator, delimiter and the rest of the
//     opening line are KEPT, so the redirect target itself is still judged.
//
// Relative paths are intentionally NOT extracted: they resolve against the cwd, which is
// already the first judged target, so listing them would judge the same directory twice.
func commandPaths(cmd string) []string {
	if strings.TrimSpace(cmd) == "" {
		return nil
	}
	var out []string
	for _, t := range commandTokens(stripHeredocBodies(cmd)) {
		// Strip the shell punctuation that glues a path to its neighbours: quotes,
		// redirects, separators, grouping.
		tok := strings.Trim(t.tok, `"'`+",;|&()<>")
		// `--flag=/path` and `VAR=/path`: keep the value half.
		if i := strings.IndexByte(tok, '='); i >= 0 {
			tok = tok[i+1:]
		}
		if tok == "" {
			continue
		}
		isHome := strings.HasPrefix(tok, "~/")
		if !isHome && !strings.HasPrefix(tok, "/") {
			continue
		}
		// A URL is not a path (`https://host/x` starts with 'h', but
		// `//host/x` and `--url=//host/x` would look absolute).
		if strings.Contains(tok, "://") || strings.HasPrefix(tok, "//") {
			continue
		}
		// Rule 2: a pattern is not a path, however it was written.
		if hasShellMetachar(tok) {
			continue
		}
		// Rule 1: a QUOTED token has to look like a path to be treated as one.
		if t.quoted && !looksLikePath(tok) {
			continue
		}
		// Rule 4: a token that is not a file to be guarded is not a consent target.
		// See harmlessDevicePath / procIntrospectionPath for why the lists are explicit.
		if harmlessDevicePath(tok) || procIntrospectionPath(tok) {
			continue
		}
		if isHome {
			home, err := os.UserHomeDir()
			if err != nil {
				continue
			}
			tok = filepath.Join(home, strings.TrimPrefix(tok, "~/"))
		}
		// An absolute path that is only the root is not a target worth judging on
		// its own — the cwd entry already covers "this command runs somewhere".
		if cleaned := filepath.Clean(tok); cleaned != "/" {
			out = append(out, cleaned)
		}
	}
	return out
}

// cmdToken is one word of a shell command, with whether it came from a QUOTED span. The
// distinction is why this is a scanner rather than strings.Fields: a quoted word is DATA
// (a program, a regex, a literal), and treating it as an argument is how an awk program's
// regex became a consent target.
type cmdToken struct {
	tok    string
	quoted bool
}

// commandTokens splits a command into words the way a shell would for the purposes of THIS
// scan: whitespace separates words, a quoted span is ONE word with its quotes removed (so a
// quoted path containing a space survives), and an escaped character does not end a word.
func commandTokens(cmd string) []cmdToken {
	var out []cmdToken
	var b strings.Builder
	var quote rune
	quoted, started, escaped := false, false, false
	flush := func() {
		if started {
			out = append(out, cmdToken{tok: b.String(), quoted: quoted})
		}
		b.Reset()
		quoted, started = false, false
	}
	for _, r := range cmd {
		switch {
		case escaped:
			b.WriteRune(r)
			escaped = false
		case quote != 0:
			if r == quote {
				quote = 0
				continue
			}
			b.WriteRune(r)
		case r == '\\':
			started = true
			escaped = true
		case r == '\'' || r == '"':
			started, quoted, quote = true, true, r
		case r == ' ' || r == '\t' || r == '\n':
			flush()
		default:
			started = true
			b.WriteRune(r)
		}
	}
	flush()
	return out
}

// hasShellMetachar reports whether a token contains a character that makes it a PATTERN
// rather than a path we could judge — a regex or glob operator, or a shell expansion.
// `$` is in the set for the expansion reason: the consent layer sees the command TEXT, so
// `/a$b` is not the path the shell would pass, and judging the literal would invent a
// target. A real path may legally contain most of these, but the two mistakes do not cost
// the same: a MISSED mention is still covered by the policy's deny list and by the
// execution guard, while an INVENTED one produces an ask nothing can answer.
func hasShellMetachar(tok string) bool {
	return strings.ContainsAny(tok, "^+!*,?$[]{}()|\\")
}

// harmlessDevicePath reports whether tok names one of the STANDARD device nodes — the
// ones a shell uses as a sink or a stream rather than as a file it is acting on:
// /dev/null above all, which appears in ordinary work constantly (`2>/dev/null`,
// `> /dev/null`, `curl -o /dev/null`).
//
// THEY ARE SKIPPED BECAUSE A CONSENT DECISION ABOUT THEM IS MEANINGLESS. "May this command
// touch /dev/null?" has one answer, and asking it is pure friction — the operator was
// granting the project directory and being asked about /dev for a command that only threw
// its stderr away.
//
// THE LIST IS EXPLICIT AND MUST STAY THAT WAY. A blanket "skip /dev" would wave through
// `rm -rf /dev/sda` and `dd of=/dev/nvme0n1`, which are exactly the catastrophic writes
// this gate exists to stop — so only the members that hold no state and cannot be damaged
// are named here. `mkfs`/`dd` are separately refused outright by the never-allow class.
func harmlessDevicePath(tok string) bool {
	switch filepath.Clean(tok) {
	case "/dev/null", "/dev/zero", "/dev/full",
		"/dev/random", "/dev/urandom",
		"/dev/stdin", "/dev/stdout", "/dev/stderr",
		"/dev/tty", "/dev/console":
		return true
	}
	return false
}

// procIntrospectionPath reports whether tok reads KERNEL METADATA about a process —
// `/proc/self/status`, `/proc/1234/exe`, `/proc/1234/maps` — rather than naming a file the
// command could act on. Reading a process's executable, its maps or its command line is
// introspection, and asking the operator for consent to LOOK is noise.
//
// DELIBERATELY NARROW, and the exclusions matter more than the inclusions:
//
//   - `/proc/sys/**` is WRITABLE kernel configuration and is never skipped.
//   - `/proc/<pid>/mem` is a direct write into another process's address space and is never
//     skipped.
//   - `/proc/<pid>/fd/**` is never skipped either, because a redirect THROUGH an fd
//     (`echo x > /proc/1234/fd/5`) writes into whatever that descriptor points at — and
//     commandPaths cannot tell a read from a write, so it must assume the dangerous case.
//
// The remaining members are read-only metadata: there is no write-through and no unlink.
func procIntrospectionPath(tok string) bool {
	clean := filepath.Clean(tok)
	rest, ok := strings.CutPrefix(clean, "/proc/")
	if !ok {
		return false
	}
	// `/proc/self/...` and `/proc/thread-self/...`, or `/proc/<digits>/...`.
	pid, leaf, found := strings.Cut(rest, "/")
	if !found || leaf == "" {
		return false
	}
	if pid != "self" && pid != "thread-self" {
		if pid == "" {
			return false
		}
		for _, r := range pid {
			if r < '0' || r > '9' {
				return false
			}
		}
	}
	switch leaf {
	case "exe", "cwd", "root", "comm",
		"cmdline", "environ", "status", "stat", "statm", "maps", "smaps",
		"io", "limits", "mountinfo", "mounts":
		return true
	}
	// A deeper path under one of those leaves (e.g. /proc/self/status/... does not exist,
	// but /proc/1234/maps is already covered). Anything else is not introspection.
	return false
}

// looksLikePath reports whether a QUOTED token is plausibly a filesystem path. A quoted
// span is data, so the bar is higher than for an unquoted argument: it must be
// home-relative or carry at least two path segments, and it may not END in a slash —
// because that is how a regex is delimited. `/func /` (a grep pattern) has two slashes and
// would otherwise pass; `/etc/b` is a path; `/return` is neither.
func looksLikePath(tok string) bool {
	if strings.HasSuffix(tok, "/") {
		return false
	}
	if strings.HasPrefix(tok, "~/") {
		return true
	}
	return strings.Count(tok, "/") >= 2
}

// stripHeredocBodies removes every heredoc BODY from a command, keeping the `<<DELIM`
// operator, the delimiter and the rest of the opening line — so the redirect target is
// still visible to commandPaths.
//
// A heredoc body is text about to be WRITTEN TO A FILE, so a path in it is not a path this
// command touches. Judging them meant that writing a test file whose CONTENT mentioned a
// /tmp path raised a consent ask about /tmp: the file's content was being read as though it
// were an argument, which is both wrong and a source of asks the operator cannot satisfy by
// granting the directory they are actually working in.
//
// LIMITS, stated rather than hidden: `<<-` (tab-stripped terminators) is not special-cased,
// and an operator after an unbalanced quote is not found. Both leave the body in the text,
// which is the CONSERVATIVE direction — those paths come back and may raise an ask, rather
// than a body being dropped that held a real argument.
func stripHeredocBodies(cmd string) string {
	var out strings.Builder
	rest := cmd
	for {
		i := indexHeredocOp(rest)
		if i < 0 {
			out.WriteString(rest)
			return out.String()
		}
		delim, headerLen, ok := parseHeredocDelim(rest[i:])
		if !ok {
			out.WriteString(rest)
			return out.String()
		}
		// Through the delimiter word, then to the end of that LINE: the body starts on
		// the next line, and the opening line may still carry a redirect.
		headerEnd := i + headerLen
		nl := strings.IndexByte(rest[headerEnd:], '\n')
		if nl < 0 {
			out.WriteString(rest) // nothing follows the operator, so there is no body
			return out.String()
		}
		out.WriteString(rest[:headerEnd+nl+1])
		rest = cutHeredocBody(rest[headerEnd+nl+1:], delim)
	}
}

// indexHeredocOp returns the index of the first `<<` that is not inside quotes, or -1.
func indexHeredocOp(s string) int {
	var quote rune
	for i, r := range s {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			}
		case r == '\'' || r == '"':
			quote = r
		case r == '<':
			if i+1 < len(s) && s[i+1] == '<' {
				return i
			}
		}
	}
	return -1
}

// parseHeredocDelim reads a heredoc operator at the head of s (`<<EOF`, `<<'EOF'`) and
// returns the delimiter plus the byte length of the operator itself.
func parseHeredocDelim(s string) (string, int, bool) {
	if !strings.HasPrefix(s, "<<") {
		return "", 0, false
	}
	j := 2
	// `<<-` strips leading tabs from the body and its terminator.
	if j < len(s) && s[j] == '-' {
		j++
	}
	for j < len(s) && (s[j] == ' ' || s[j] == '\t') {
		j++
	}
	if j >= len(s) || s[j] == '\n' {
		return "", 0, false
	}
	if s[j] == '\'' || s[j] == '"' {
		quote := s[j]
		j++
		start := j
		for j < len(s) && s[j] != quote {
			j++
		}
		if j >= len(s) || j == start {
			return "", 0, false
		}
		return s[start:j], j + 1, true
	}
	start := j
	for j < len(s) && s[j] != ' ' && s[j] != '\t' && s[j] != '\n' {
		j++
	}
	if j == start {
		return "", 0, false
	}
	return s[start:j], j, true
}

// cutHeredocBody returns what follows the first line equal to delim. An unterminated
// heredoc consumes the rest of the command, which is also what a shell would do.
func cutHeredocBody(s, delim string) string {
	for {
		nl := strings.IndexByte(s, '\n')
		if nl < 0 {
			return ""
		}
		if strings.TrimSuffix(s[:nl], "\r") == delim {
			return s[nl+1:]
		}
		s = s[nl+1:]
	}
}

// commandInvokesScopedBinary reports whether a shell command invokes one of the binaries the
// execution guard intercepts, which is the set whose TARGETS the shim judges.
//
// IT IS DELIBERATELY A TOKEN SCAN RATHER THAN A PARSE, and it is not the enforcement: the shim
// intercepts by PATH lookup, so it sees an invocation however it was built (`$(echo rm) -rf x` still
// resolves through the shimmed PATH). This only decides whether the consent layer should apply the
// protected-path rule EARLY, and a scan that errs toward "yes" is the safe direction for that — the
// cost of a false positive is a refusal of something the shim would refuse anyway.
//
// An assignment (`HOME=/home/me`) and a bare mention (`cat /home/me/file`) are NOT invocations of a
// scoped binary, which is exactly the distinction the ungated version was missing.
func commandInvokesScopedBinary(cmd string) bool {
	scoped := guard.ScopedBinaryNames()
	if len(scoped) == 0 || strings.TrimSpace(cmd) == "" {
		return false
	}
	for _, tok := range strings.Fields(cmd) {
		// Strip the shell punctuation that glues a token to its neighbours, and any PATH prefix, so
		// `/bin/rm`, `"rm` and `rm;` all read as the binary `rm`.
		tok = strings.Trim(tok, `"'`+"`"+`,;|&()<>{}[]$`)
		if tok == "" || strings.Contains(tok, "=") && !strings.HasPrefix(tok, "/") {
			// An assignment is not a command. (A quoted path may contain '='; the prefix test keeps
			// an absolute path from being discarded here.)
			if !strings.Contains(tok, "/") {
				continue
			}
		}
		base := tok
		if i := strings.LastIndexByte(base, '/'); i >= 0 {
			base = base[i+1:]
		}
		for _, n := range scoped {
			if base == n {
				return true
			}
		}
	}
	return false
}

// decisionTargets returns EVERY path a file action touches, so the decision
// covers a batch write's second (and later) targets instead of only the first.
// Judging only the first is the silent approval the MCP-ask fix closed, one
// path over: a batch_write whose first path is inside the conversation's
// project and whose second is a sibling path would ride the first path's
// verdict. This is C4's own rationale — "a second directory in the batch asks
// again". The grant KEY stays the first target's directory (C4); each target is
// judged on its own absolute path (C5: deny names files) with its own
// directory's grant read.
//
// A bash action is not path-scopable (C4), so it yields the single scope-dir
// entry, as does an action that resolved no target at all (the fail-closed
// card case).
func decisionTargets(a askAction, dir string) []decisionTarget {
	if isBashAsk(a.Tool) {
		cwd := filepath.Clean(dir)
		out := []decisionTarget{{abstarget: cwd, key: cwd}}
		// ALSO JUDGE THE PATHS THE COMMAND NAMES.
		//
		// A shell command's consent KEY is its cwd, so one "allow for this session"
		// on the project directory would otherwise cover every command run there —
		// including `cp ~/secret /etc/x`, which touches nothing inside the project.
		// The operator's decision: "also judge the paths a command mentions."
		//
		// The cwd stays FIRST because it is the grant KEY (C4): a grant is still
		// given for the directory the command runs in. Each mentioned path is then
		// judged on its own absolute value, so a path outside every granted
		// directory raises an ask of its own.
		seen := map[string]bool{cwd: true}
		for _, p := range commandPaths(a.Command) {
			if seen[p] {
				continue
			}
			seen[p] = true
			out = append(out, decisionTarget{abstarget: p, key: filepath.Dir(p)})
		}
		return out
	}
	if len(a.Targets) == 0 {
		d := filepath.Clean(dir)
		return []decisionTarget{{abstarget: d, key: d}}
	}
	out := make([]decisionTarget, 0, len(a.Targets))
	for _, t := range a.Targets {
		t = filepath.Clean(t)
		if !filepath.IsAbs(t) {
			t = filepath.Join(filepath.Clean(dir), t)
		}
		out = append(out, decisionTarget{abstarget: t, key: filepath.Dir(t)})
	}
	return out
}

// askSummary states WHAT THE CALL WILL DO and to WHAT, in the operator's terms:
// "modify /p/a.go", "run a shell command: go test ./...". It is the whole
// content of the decision — "approve" is meaningless without it — so it must
// never leak a raw argument dump. An earlier version concatenated the parsed
// args with fmt's %v, which put a Go struct literal on an operator's card
// ("batch_write writes=[map[mode:edit new:type chatBus struct {…]") for any
// tool whose args are nested. See scalarArg.
func askSummary(a askAction) string {
	verb := toolIntentVerb(a.Tool)
	var out string
	switch {
	case isBashAsk(a.Tool) && a.Command != "":
		// Returned early: the verb already names the tool class for a shell ask
		// ("run a shell command"), so the tool suffix below would add nothing.
		return verb + ": " + truncateArg(a.Command)
	case len(a.Targets) > 0:
		out = verb + " " + strings.Join(a.Targets, ", ")
	case a.Command != "":
		out = verb + ": " + truncateArg(a.Command)
	case a.Tool != "" && len(a.Input) > 0:
		// The args are known but no target could be read out of them: show the
		// intent and a SAFE summary of the args (compactArgs never dumps).
		out = verb + " " + compactArgs(a.Input)
	default:
		out = verb
	}
	// THE TOOL NAME STAYS ON THE CARD, beside the intent. The verb says what
	// will happen; the tool says who does it, and an operator diagnosing an
	// unexpected ask wants both ("modify /p/a.go (batch_write)" reads as one
	// action, where either half alone is ambiguous about the other). Skipped
	// when the text already contains it.
	if a.Tool != "" && !strings.Contains(out, a.Tool) {
		out += " (" + a.Tool + ")"
	}
	return out
}

// toolIntentVerb maps a tool name onto what the call will DO, in plain terms.
// The ask is answered by a human, so the verb is the point: "batch_write" says
// nothing to an operator, "modify" does. Matching is on the tool name because
// the vocabulary is open (opencode, MCP, and the native suite all name their
// tools differently), so the classes are deliberately broad.
func toolIntentVerb(tool string) string {
	t := strings.ToLower(strings.TrimSpace(tool))
	switch {
	case isBashAsk(t):
		return "run a shell command"
	case strings.Contains(t, "write"), strings.Contains(t, "edit"),
		strings.Contains(t, "patch"), strings.Contains(t, "create"):
		return "modify"
	case strings.Contains(t, "delete"), strings.Contains(t, "remove"), t == "rm":
		return "delete"
	case strings.Contains(t, "read"), strings.Contains(t, "view"),
		strings.Contains(t, "list"), strings.Contains(t, "glob"),
		strings.Contains(t, "grep"), strings.Contains(t, "search"),
		strings.Contains(t, "fetch"):
		return "read"
	case strings.Contains(t, "mcp"):
		return "run a tool"
	}
	return "use the tool"
}

// ---------------------------------------------------------------------------
// The grant store (in-memory, per conversation, directory-keyed)
// ---------------------------------------------------------------------------

// grantStore records session grants: "allow for this directory" answers. It is
// deliberately IN MEMORY with no persistence (C9): a plane restart clears it,
// and a new conversation asks again. Keyed conversation id -> cleaned directory,
// valued with the time the grant was recorded (the client lists grants and says
// when each was given).
type grantStore struct {
	mu     sync.Mutex
	byConv map[string]map[string]time.Time
}

func newGrantStore() *grantStore {
	return &grantStore{byConv: make(map[string]map[string]time.Time)}
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
		set = make(map[string]time.Time)
		g.byConv[convID] = set
	}
	set[filepath.Clean(dir)] = time.Now()
}

// Has reports whether convID holds a grant covering dir.
//
// A GRANT COVERS ITS SUBTREE. "Allow for this session" on a directory means that
// directory AND everything under it — a grant was matched by EXACT string before,
// so granting a project root did not cover its packages and the operator got a
// card per directory. That is precisely the per-command friction that pushes
// people to approve without reading, which is the failure the gate exists to
// prevent.
//
// The separator matters: a prefix test alone would make /foo cover /foobar. The
// grant must be the target itself or a proper ancestor of it.
func (g *grantStore) Has(convID, dir string) bool {
	if g == nil || convID == "" || strings.TrimSpace(dir) == "" {
		return false
	}
	target := filepath.Clean(dir)
	g.mu.Lock()
	defer g.mu.Unlock()
	set := g.byConv[convID]
	if len(set) == 0 {
		return false
	}
	for granted := range set {
		if target == granted || strings.HasPrefix(target, granted+string(filepath.Separator)) {
			return true
		}
	}
	return false
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

// Roots returns the conversation's session-granted directories, cleaned and
// sorted. It is the guard shim's read of "the directories this conversation may
// write to": the interactive profile honours these in addition to the
// conversation's project. Nil when the conversation holds nothing.
func (g *grantStore) Roots(convID string) []string {
	if g == nil || convID == "" {
		return nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	set := g.byConv[convID]
	if len(set) == 0 {
		return nil
	}
	out := make([]string, 0, len(set))
	for dir := range set {
		out = append(out, dir)
	}
	sort.Strings(out)
	return out
}

// sessionGrant is one active session grant as the wire presents it: the granted
// directory and when it was granted.
type sessionGrant struct {
	Directory string
	GrantedAt time.Time
}

// Revoke drops one session grant for a conversation. It reports whether a grant
// was actually removed, so an unknown directory is never a silent success. The
// next tool call for that directory asks again: the guard shim reads Roots on
// every decision (ask_guard.go), so there is no cache to invalidate.
func (g *grantStore) Revoke(convID, dir string) bool {
	if g == nil || convID == "" || strings.TrimSpace(dir) == "" {
		return false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	set := g.byConv[convID]
	if set == nil {
		return false
	}
	if _, ok := set[filepath.Clean(dir)]; !ok {
		return false
	}
	delete(set, filepath.Clean(dir))
	if len(set) == 0 {
		delete(g.byConv, convID)
	}
	return true
}

// List returns the conversation's active grants sorted by directory (stable
// order for the client's list). Nil when the conversation holds nothing.
func (g *grantStore) List(convID string) []sessionGrant {
	if g == nil || convID == "" {
		return nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	set := g.byConv[convID]
	if len(set) == 0 {
		return nil
	}
	out := make([]sessionGrant, 0, len(set))
	for dir, at := range set {
		out = append(out, sessionGrant{Directory: dir, GrantedAt: at})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Directory < out[j].Directory })
	return out
}

// ---------------------------------------------------------------------------
// The once-target store (in-memory, per conversation)
// ---------------------------------------------------------------------------

// onceStore records the ABSOLUTE targets the operator answered ALLOW_ONCE for,
// per conversation. It exists because the shim cannot ask: when the consent core
// answers an ask with `once`, the very command the operator just approved must
// not then be refused by the guard shim — while a SIBLING path a subprocess
// inside it targets still is. In memory like grantStore (a restart clears it; a
// new conversation asks again).
type onceStore struct {
	mu     sync.Mutex
	byConv map[string]map[string]bool
}

func newOnceStore() *onceStore {
	return &onceStore{byConv: make(map[string]map[string]bool)}
}

// Record arms the approved absolute targets for convID. Nil-safe.
func (o *onceStore) Record(convID string, abs ...string) {
	if o == nil || convID == "" {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	set := o.byConv[convID]
	if set == nil {
		set = make(map[string]bool)
		o.byConv[convID] = set
	}
	for _, t := range abs {
		t = strings.TrimSpace(t)
		if t == "" {
			continue
		}
		set[filepath.Clean(t)] = true
	}
}

// Targets returns the conversation's approved once-targets, sorted. Nil-safe.
func (o *onceStore) Targets(convID string) []string {
	if o == nil || convID == "" {
		return nil
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	set := o.byConv[convID]
	if len(set) == 0 {
		return nil
	}
	out := make([]string, 0, len(set))
	for t := range set {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

// ClearConversation drops a conversation's once-targets (conversation end).
func (o *onceStore) ClearConversation(convID string) {
	if o == nil {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	delete(o.byConv, convID)
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
	// Summary is the one-line card text (never just an opaque id).
	Summary string
	// DenyBelow are the operator's persistent DENY entries at or below
	// Directory: a session grant cannot override them, so the card says so
	// instead of implying the grant covers everything under the directory.
	DenyBelow []string
	// AbsTargets are the ABSOLUTE paths this ask's decision covered
	// (decisionTargets), recorded so an ALLOW_ONCE reply can arm exactly those
	// paths in the execution guard's shim (the shim cannot ask).
	AbsTargets []string

	// --- A QUESTION ask (ask_user, blocking) ---
	//
	// Question non-empty marks this ask as a clarifying question rather than a
	// permission decision: there is no grant, no allow/deny and no precedence chain,
	// so it is not run through decide() at all. It rides the same registry, the same
	// card and the same reply RPC because those are what make it answerable — adding
	// a second path would have meant a second place for a reply to go missing.
	Question   string
	Options    []string
	AllowOther bool
	// Answer is a question ask's reply: the chosen label, or the operator's own
	// words. It becomes the ask_user TOOL RESULT, so the model continues the turn
	// holding the answer rather than being told the question was recorded.
	Answer string

	// reply wakes the drain loop's select when a decision lands.
	reply chan struct{}

	mu     sync.Mutex
	state  askState
	choice apiv1.PermissionChoice
	// answer holds a QUESTION ask's reply (see recordClientAnswer). Separate from
	// choice: a permission's outcome is a grant decision, a question's is content
	// the model reads, and conflating them would let one masquerade as the other.
	answer string
	// autoApproved marks a decision made by FULLSEND rather than by the operator, so the
	// recorded verdict can say so. The decision is genuinely the same (ALLOW_ONCE), but a
	// transcript that reports it as `user_PERMISSION_CHOICE_ALLOW_ONCE` claims the operator
	// clicked something they never saw — and this codebase's rule is that a decision record
	// must not misattribute who decided.
	autoApproved bool
}

// autoApproveOnce records an ALLOW_ONCE that the OPERATOR did not make: fullsend cleared a
// card that was already on screen. False when the ask was no longer open (someone answered
// it first, or it settled), so the caller cannot report an approval that did not happen.
func (a *pendingAsk) autoApproveOnce() bool {
	if !a.clientReply(apiv1.PermissionChoice_PERMISSION_CHOICE_ALLOW_ONCE) {
		return false
	}
	a.mu.Lock()
	a.autoApproved = true
	a.mu.Unlock()
	return true
}

// wasAutoApproved reports whether fullsend made this decision rather than the operator.
func (a *pendingAsk) wasAutoApproved() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.autoApproved
}

// isQuestion reports whether this ask is a clarifying question rather than a
// permission decision.
func (a *pendingAsk) isQuestion() bool { return strings.TrimSpace(a.Question) != "" }

// recordClientAnswer records a QUESTION's answer. Same open-check as clientReply:
// false when the ask is no longer open, so a late answer reports expired rather
// than pretending to have been delivered.
func (a *pendingAsk) recordClientAnswer(text string) bool {
	a.mu.Lock()
	if a.state != askOpen {
		a.mu.Unlock()
		return false
	}
	a.state = askClientReplied
	a.answer = text
	a.mu.Unlock()
	select {
	case a.reply <- struct{}{}:
	default:
	}
	return true
}

// clientAnswerValue reports the recorded answer, if one landed.
func (a *pendingAsk) clientAnswerValue() (string, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.state == askClientReplied && a.answer != "" {
		return a.answer, true
	}
	return "", false
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
	return apiv1.PermissionChoice_PERMISSION_CHOICE_UNSPECIFIED, false
}

// isOpen reports whether the ask is still awaiting a human decision. The
// re-attach recovery path replays only these; a decided or finalized ask stays
// settled (its outcome is already in the transcript).
func (a *pendingAsk) isOpen() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.state == askOpen
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
		return apiv1.PermissionChoice_PERMISSION_CHOICE_UNSPECIFIED, false, true
	default:
		return apiv1.PermissionChoice_PERMISSION_CHOICE_UNSPECIFIED, false, false
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
	case apiv1.PermissionChoice_PERMISSION_CHOICE_ALLOW_ONCE, apiv1.PermissionChoice_PERMISSION_CHOICE_ALLOW_SESSION:
		return "once", true
	case apiv1.PermissionChoice_PERMISSION_CHOICE_DENY:
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
	ct.ledger.recordPermission(a.Tool, target, verdict, detail, a.AskID)
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
	// STAMP THE ASK ID FIRST, so every record this decision writes carries it — including
	// the `never_allow` and `policy_error` refusals below, which are decided before any card
	// is raised and are still decisions about THIS ask.
	a.AskID = evt.PermissionID
	if r := binaryClassRefusal(a); r != "" {
		ct.record(a, "never_allow", r)
		return "reject", nil, r
	}
	scope := ct.scopeFor(ctx)
	a.Key = resolveAskKey(a, scope.Dir)

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

	// EVERY target the action touches is judged — not just the one that names
	// the KEY (see decisionTargets). The KEY is unchanged (C4), so grants and
	// the card do not move.
	pol := askPermissionPolicy()
	proceed := resolved
	allInside := resolved
	verdict := permpolicy.VerdictAsk
	targets := decisionTargets(a, scope.Dir)
	absTargets := make([]string, 0, len(targets))
	for _, t := range targets {
		absTargets = append(absTargets, t.abstarget)
	}
	// A TARGET THAT WOULD DESTROY THE SCOPE IS REFUSED, NOT ASKED ABOUT — and this is the layer where
	// "asked about" would be the worst outcome, because a card offers the operator a button that
	// cannot be taken back.
	//
	// WHERE IT SITS: above the policy loop and above FULLSEND, in the same position as the never-allow
	// class and for the same reason — it is a decision, not a permission request. A DENY entry is
	// consulted first: an operator's own exclusion is their decision to state, and it should be the
	// reason they are given.
	//
	// WHAT IT CATCHES: `rm -rf /home` from a project, `rm -rf ~`, `rm -rf /`, and `rm -rf` of any
	// directory that CONTAINS the project — every one of which would take the work scope with it. It
	// does NOT refuse acting on the scope root itself (`chmod -R 755 <project>`, `rm -rf <project>/dist`),
	// which is ordinary work; see protectedpath's two lists.
	//
	// IT IS THE CONSENT HALF OF THE SAME RULE the guard shim enforces, from ONE declaration
	// (internal/protectedpath), so the two cannot disagree about what is protected.
	protRoots := protectedpath.Roots("")
	protScope := protectedpath.ScopeRoots(scope.Dir, ct.svc.grants.Roots(ct.convID))
	//
	// GATED ON THE COMMAND ACTUALLY INVOKING A BINARY THE SHIM JUDGES, because this layer judges
	// MENTIONS and the shim judges OPERATIONS. For a bash command the targets include every literal
	// path the TEXT names — that is the point of the extraction, and it is what catches
	// `cp ~/a /etc/b` — so a command that merely mentions a protected path was refused as though it
	// were deleting it: `HOME=/home/me somecmd` reads as a target that contains the home directory.
	// The shim would never look at that command (it intercepts rm/mv/cp/... and nothing else), so
	// refusing it here was over-refusal and a false statement about the rule.
	//
	// The SHIM REMAINS THE ENFORCEMENT: it intercepts an invocation by PATH LOOKUP, so it covers
	// forms the text does not literally contain. This layer's job is to refuse EARLY rather than offer
	// a card for something no approval could permit.
	if !isBashAsk(a.Tool) || commandInvokesScopedBinary(a.Command) {
		for _, t := range targets {
			if root := protectedpath.DestroyedBy(t.abstarget, protRoots, protScope); root != "" {
				ref := protectedpath.Refusal(t.abstarget, root)
				ct.record(a, "protected_path", ref)
				return "reject", nil, ref
			}
		}
	}
	// blocking is the FIRST target that is not covered — the path whose consent is
	// actually missing, and therefore the directory a grant has to name to silence this
	// ask. See the card's key below for why it, and not the cwd, is what the card states.
	var blocking *decisionTarget
	for i, t := range targets {
		d, err := pol.Decide(t.abstarget, permpolicy.Inputs{
			SessionGranted: ct.svc.grants.Has(ct.convID, t.key),
		})
		if err != nil {
			// Fail closed: a malformed policy file must never silently proceed.
			ct.record(a, "policy_error", err.Error())
			return "reject", nil, err.Error()
		}
		if d.Verdict == permpolicy.VerdictDeny {
			ref := pol.Refusal(t.abstarget, d.Entry).Error()
			ct.record(a, "deny", ref)
			return "reject", nil, ref
		}
		if i == 0 {
			// The transcript records the KEY target's verdict — the directory a
			// grant would cover.
			verdict = d.Verdict
		}
		if !d.Verdict.Proceed() {
			proceed = false
			if blocking == nil {
				bt := t
				blocking = &bt
			}
		}
		if !scope.PreApprovedPath(t.abstarget) {
			allInside = false
		}
	}
	if proceed {
		// grant | accept | project covers every target — proceed silently.
		ct.record(a, verdict.String(), "")
		return "once", nil, ""
	}
	// FULLSEND: the operator has waived the PROMPT for this conversation, so an action
	// that would have raised a card proceeds instead.
	//
	// WHERE THIS SITS IS THE WHOLE DESIGN, so it is worth being precise about what has
	// already happened by the time control reaches here:
	//
	//   - binaryClassRefusal ran FIRST (the top of this function), so sudo / dd / mkfs*
	//     are already refused and fullsend cannot reach them. That class is not a
	//     permission, so there is no permission to waive.
	//   - the policy has been consulted for EVERY target and a DENY verdict has already
	//     returned `reject` above. A deny is a decision the policy MADE — no card is
	//     raised for it — so again there is nothing here to waive. Fullsend does not
	//     open ~/.ssh, and a mode that did would be one whose name promises more than it
	//     does in the direction that matters.
	//   - a malformed policy has already failed closed.
	//
	// So what is left to waive is exactly the ASK. It covers an UNRESOLVED action too,
	// deliberately: an ask we could not describe is still an ask, and fullsend is the
	// operator saying "stop asking me" — not "ask me only when you can name the target".
	//
	// The transcript records `fullsend`, so scrolling back shows WHY no card appeared
	// rather than a silently missing decision.
	if ct.svc.fullsend.Enabled(ct.convID) {
		ct.record(a, "fullsend", "")
		return "once", nil, ""
	}
	if !resolved {
		ct.log().Warn("ask orchicon consent: the ask carries no target path or command — asking rather than proceeding",
			"conversation", ct.convID, "ask", evt.PermissionID, "tool", a.Tool)
	}

	// VerdictAsk (or an unresolved action): record, emit the card, await the
	// human.
	// The summary states the INTENT (askSummary). An unresolved action — no
	// path or command could be extracted — is still described by its verb, so
	// the card never explains itself in developer terms; the diagnostic for that
	// case belongs in the Warn above, not in copy an operator reads.
	summary := askSummary(a)
	// THE CARD NAMES THE DIRECTORY WHOSE CONSENT IS ACTUALLY MISSING, which is not always
	// the cwd, and the difference is the difference between an answerable card and a
	// treadmill.
	//
	// A shell command's KEY is its cwd (C4), so the card used to say "never ask again in
	// <the project>" for a command that the project grant could never silence — because
	// what forced the ask was a MENTIONED path outside it (a redirect to a log file, a
	// temp dir). The operator granted the named directory, was asked again by the next
	// command, and had no way to see why: the card kept naming a directory that was
	// already granted. Naming the BLOCKING target instead makes the offered grant the one
	// that works, and it is also the honest description of the ask — this is the path
	// Orchicon does not have consent for.
	//
	// When nothing blocks (the fail-closed unresolved case, where the target IS the scope
	// dir) this is the scope dir and the card is unchanged.
	grantDir := a.Key
	if blocking != nil && blocking.key != "" {
		grantDir = blocking.key
	}
	ask = &pendingAsk{
		AskID:          evt.PermissionID,
		ConversationID: ct.convID,
		SessionID:      sid,
		Action:         a,
		Key:            grantDir,
		Tool:           a.Tool,
		Command:        a.Command,
		Targets:        a.Targets,
		Directory:      grantDir,
		// The card claims "inside the project" only when EVERY target is; an
		// unresolved action has no known target at all, so it cannot.
		InsideProject: allInside,
		Summary:       summary,
		DenyBelow:     ct.denyBelowForGrant(pol, grantDir),
		AbsTargets:    absTargets,
		reply:         ct.replies,
	}
	ct.svc.pending.put(ct.convID, ask)
	if ct.monitor != nil {
		ct.monitor.setAwaitingConsent(true)
	}
	ct.record(a, "ask", summary)
	return "", ask, ""
}

// denyBelowForGrant lists the operator's DENY entries that key's directory would
// NOT override (permpolicy.Store.DenyBelow) — the entries a session grant still
// loses to, which the card names. A malformed policy must never fail the ask
// (the decision path already fails closed above): the error is logged and the
// list reported empty, so the card simply omits the precedence note.
func (ct *consentTurn) denyBelowForGrant(pol *permpolicy.Store, key string) []string {
	if pol == nil || strings.TrimSpace(key) == "" {
		return nil
	}
	entries, err := pol.DenyBelow(key)
	if err != nil {
		ct.log().Warn("ask orchicon consent: could not list deny entries below the grant directory",
			"conversation", ct.convID, "directory", key, "error", err)
		return nil
	}
	return entries
}

// raiseQuestion records a clarifying question the model asked and the turn is
// PAUSED on, and returns it for the client to render.
//
// NO POLICY RUNS. A question is not a permission: there is no target to judge, no
// grant to record and no never-allow class to consult. Routing it through decide()
// would have asked the operator for PERMISSION TO ASK A QUESTION — which is why it
// gets its own path rather than a special case inside the precedence chain.
//
// The question's own arguments ride evt.InputJSON when the adapter has not parsed
// them, so the SAME validator the tool uses decides what is askable: a prompt that
// is not a question (no text, or a single option with no free-text row) is refused
// here with the tool's own error rather than parked as an unanswerable card.
func (ct *consentTurn) raiseQuestion(sid string, evt scheduler.SessionEvent) (*pendingAsk, string) {
	if strings.TrimSpace(evt.PermissionID) == "" {
		// Without an id the answer could never be correlated, so this must fail
		// loudly rather than park a card nobody can answer.
		return nil, "ask_user: the adapter raised a question with no ask id"
	}
	question := strings.TrimSpace(evt.Question)
	options := append([]string(nil), evt.Options...)
	allowOther := evt.AllowOther
	if question == "" && strings.TrimSpace(evt.InputJSON) != "" {
		q, opts, ao, err := parseAskUserArgs([]byte(evt.InputJSON))
		if err != nil {
			return nil, err.Error()
		}
		question = q
		allowOther = ao
		if len(options) == 0 {
			for _, o := range opts {
				options = append(options, o.Label)
			}
		}
	}
	if question == "" {
		return nil, "ask_user: the question carries no text"
	}
	a := &pendingAsk{
		AskID:          evt.PermissionID,
		ConversationID: ct.convID,
		SessionID:      sid,
		Tool:           "ask_user",
		Question:       question,
		Options:        options,
		AllowOther:     allowOther,
		Summary:        "asking: " + truncateArg(question),
		reply:          ct.replies,
	}
	ct.svc.pending.put(ct.convID, a)
	if ct.monitor != nil {
		// A human reading the card is not a wedged tool.
		ct.monitor.setAwaitingConsent(true)
	}
	ct.record(a.Action, "question", question)
	return a, ""
}

// applyClientReplies answers the serve for every ask whose client decision has
// landed. Called from the drain loop's reply arm.
// askResolution is one decision this collector APPLIED, ready to be published on the
// turn's stream so every watcher settles its copy of the card. See
// apiv1.PermissionAskResolved.
type askResolution struct {
	AskID   string
	Outcome string
	Answer  string
}

// applyClientReplies answers the serve for every ask whose client decision has
// landed, and returns what it APPLIED so the caller can publish each resolution on
// the turn's stream.
//
// THE RETURN VALUE IS THE POINT: an ask reaches EVERY watcher of a turn, but only
// the client that answered it cleared its own copy — so answering in the TUI left the
// GUI showing a live-looking, inert card (the operator: "the choice box is still there
// for permissions"), and the clients cannot infer it, because a permission ask has no
// durable per-ask row to reconcile against. Publishing the outcome is the only way a
// watching client learns that someone else decided.
func (ct *consentTurn) applyClientReplies(ctx context.Context, client scheduler.ChatTurnClient) []askResolution {
	var out []askResolution
	for _, a := range ct.svc.pending.list(ct.convID) {
		// A QUESTION's reply is CONTENT, not a choice. It goes to the adapter as the
		// decision string, which the adapter returns as the ask_user tool result —
		// so the model resumes the turn holding the answer.
		if ans, ok := a.clientAnswerValue(); ok {
			if err := client.ReplyPermissionDecision(ctx, a.SessionID, a.AskID, ans); err != nil {
				ct.log().Warn("ask orchicon consent: question reply failed",
					"conversation", ct.convID, "ask", a.AskID, "error", err)
			}
			ct.record(a.Action, "answered", ans)
			ct.svc.pending.remove(ct.convID, a.AskID)
			out = append(out, askResolution{AskID: a.AskID, Outcome: "answered", Answer: ans})
			continue
		}
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
		// NAME THE DECIDER. A card fullsend cleared is recorded as fullsend's decision, not
		// as a click the operator never made.
		verdict := "user_" + choice.String()
		if a.wasAutoApproved() {
			verdict = "fullsend_approved"
		}
		ct.record(a.Action, verdict, "")
		if choice == apiv1.PermissionChoice_PERMISSION_CHOICE_ALLOW_ONCE {
			// Arm the approved absolute paths for the execution guard's shim:
			// the command the operator just approved must run, while a sibling
			// path a subprocess inside it targets is still refused.
			ct.svc.once.Record(ct.convID, a.AbsTargets...)
		}
		ct.svc.pending.remove(ct.convID, a.AskID)
		out = append(out, askResolution{AskID: a.AskID, Outcome: resolutionOutcome(choice)})
	}
	ct.settleMonitor()
	return out
}

// resolutionOutcome names a permission choice for the wire. It mirrors the value
// outcomeFromChoice derives on the clients, so the two sides describe a decision the
// same way.
func resolutionOutcome(c apiv1.PermissionChoice) string {
	switch c {
	case apiv1.PermissionChoice_PERMISSION_CHOICE_ALLOW_SESSION:
		return "allow_session"
	case apiv1.PermissionChoice_PERMISSION_CHOICE_DENY:
		return "deny"
	default:
		return "allow_once"
	}
}

// emitAskResolution publishes one applied decision on the turn's stream.
func emitAskResolution(emit func(*apiv1.ChatStreamResponse), convID string, r askResolution) {
	if emit == nil {
		return
	}
	emit(&apiv1.ChatStreamResponse{
		Event: &apiv1.ChatStreamResponse_PermissionAskResolved{
			PermissionAskResolved: &apiv1.PermissionAskResolved{
				AskId:          r.AskID,
				ConversationId: convID,
				Outcome:        r.Outcome,
				Answer:         r.Answer,
			},
		},
	})
}

// finalize ends the turn's consent state (C8): a client decision that landed
// but was not yet applied IS applied; every still-open ask is answered
// `reject` so the serve's session is not left holding a phantom permission;
// a late client reply then reports expired (never silence).
// finalize ends the turn's consent state (C8): a client decision that landed but was not yet
// applied IS applied; every still-open ask is answered `reject` so the serve's session is not
// left holding a phantom permission; a late client reply then reports expired (never
// silence).
//
// IT PUBLISHES WHAT IT SETTLES, which is what stops a card outliving its ask. Every client
// watching the turn holds its own copy of the card, and only the client that ANSWERED one
// clears it — so a card settled HERE (a decision that arrived as the turn ended, or an ask
// that ran out of time) used to stay on screen in every other client as a live-looking
// choice, clickable and inert. That is the operator's "the GUI and TUI are still not in
// sync": the resolution was published when a decision was APPLIED mid-turn
// (applyClientReplies) and never when an ask was settled at the end.
//
// emit may be nil (the legacy path has no client stream), in which case nothing is
// published and behaviour is unchanged.
func (ct *consentTurn) finalize(ctx context.Context, client scheduler.ChatTurnClient, emit func(*apiv1.ChatStreamResponse)) {
	for _, a := range ct.svc.pending.removeConversation(ct.convID) {
		choice, applyClient, wasOpen := a.resolveForFinalize()
		switch {
		case applyClient:
			// A QUESTION's late reply is the operator's WORDS, not a permission choice, and
			// it settles the card the same way — so it is published with its answer too.
			if ans, ok := a.clientAnswerValue(); ok {
				ct.record(a.Action, "answered", ans)
				emitAskResolution(emit, ct.convID, askResolution{AskID: a.AskID, Outcome: "answered", Answer: ans})
				continue
			}
			resp, ok := consentResponse(choice)
			if !ok {
				continue
			}
			if err := client.ReplyPermissionDecision(ctx, a.SessionID, a.AskID, resp); err != nil {
				ct.log().Warn("ask orchicon consent: post-turn reply failed",
					"conversation", ct.convID, "ask", a.AskID, "error", err)
			}
			ct.record(a.Action, "user_"+choice.String(), "")
			emitAskResolution(emit, ct.convID, askResolution{AskID: a.AskID, Outcome: resolutionOutcome(choice)})
		case wasOpen:
			if err := client.ReplyPermissionDecision(ctx, a.SessionID, a.AskID, "reject"); err != nil {
				ct.log().Warn("ask orchicon consent: expiring unanswered ask failed",
					"conversation", ct.convID, "ask", a.AskID, "error", err)
			}
			ct.record(a.Action, "expired", "unanswered at turn end")
			// "expired" is the outcome the wire documents for an ask nobody answered, and it
			// is what makes a watching client's card read as expired rather than pending.
			emitAskResolution(emit, ct.convID, askResolution{AskID: a.AskID, Outcome: "expired"})
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
	emit(permissionAskEvent(a))
}

// permissionAskEvent builds the one stream message that carries an ask, so the
// live emit path and the re-attach replay path send the identical shape.
func permissionAskEvent(a *pendingAsk) *apiv1.ChatStreamResponse {
	if a == nil {
		return nil
	}
	return &apiv1.ChatStreamResponse{
		Event: &apiv1.ChatStreamResponse_PermissionAsk{
			PermissionAsk: &apiv1.PermissionAsk{
				AskId:            a.AskID,
				ConversationId:   a.ConversationID,
				SessionId:        a.SessionID,
				Tool:             a.Tool,
				Command:          a.Command,
				Targets:          a.Targets,
				Directory:        a.Directory,
				InsideProject:    a.InsideProject,
				Summary:          a.Summary,
				DenyEntriesBelow: a.DenyBelow,
				// The question fields: set for a question ask, empty otherwise. The
				// clients key on a non-empty Question to render the question card and
				// to answer with CONTENT rather than a permission choice.
				Question:   a.Question,
				Options:    a.Options,
				AllowOther: a.AllowOther,
			},
		},
	}
}

// logAsk is a small diagnostic used by the drain loops when a card is emitted.
func (ct *consentTurn) logAsk(a *pendingAsk) {
	ct.log().Info("ask orchicon consent: awaiting a decision",
		"conversation", ct.convID, "ask", a.AskID, "tool", a.Tool,
		"directory", a.Directory, "summary", a.Summary)
}
