package antigravity

// modegate.go — the agy transport's ENFORCEMENT of the Ask mode boundary.
//
// THE BOUNDARY IS THE PLATFORM'S, NOT THIS ADAPTER'S. internal/askmode owns the mode table (Brainstorm
// denies write/edit/bash and may not act; Quick Work the same; Iteration denies nothing), and
// internal/claude/modegate.go is the claude transport's enforcement of it. This file is the antigravity
// translation of the SAME table onto agy's tool vocabulary, and it holds no policy of its own:
//
//	agy spelling            policy name     denied by
//	run_command             bash            Brainstorm, Quick Work
//	write_file / write_to_file / create_file         write    Brainstorm, Quick Work
//	edit_file / replace_file_content / …             edit     Brainstorm, Quick Work
//	mcp_<other server>_<tool>                        (opaque) Brainstorm, Quick Work
//	mcp_orchicon_<tool>                              the platform's own — never opaque
//	ask_question, view_file, grep_search, …          nothing here
//
// WHERE IT RUNS, AND WHY THAT ORDER MATTERS. The gate is applied in the permission handler BEFORE a card
// is raised (ask_permission.go). A refused tool therefore NEVER produces a permission event: the operator
// is not asked to approve something the platform has already decided against, and — the part that
// matters for the model — the refusal is delivered as a rejection with a reason it can read, exactly as
// claude's hook delivers it.
//
// THE DIFFERENCE FROM CLAUDE, STATED PLAINLY. claude's gate reads a MODE FILE written per turn, because
// its hook is a separate process that cannot see this one's memory. agy's permission request arrives in
// THIS process, so the mode is read from the session (captured from the turn's stamped context) and there
// is no file to keep in sync — one fewer moving part, and no window where a mode switch has not reached
// a helper process.
//
// THE HARDENING HALF. Where agy accepts it, the denied tools are ALSO refused before agy runs them, via
// `--settings` (session-only, never saved: verified against agy 1.5.13's own `--help`). That is defence in
// depth and NOT the authority: the mode can change between turns while argv is fixed at spawn, so the
// in-handler gate is what actually holds. See askPermissionHandler.
//
// THE `--settings` DENY LIST IS INERT ON agy 1.5.13 AND THE SESSION MODE IS NOT. Measured against the real
// binary: a `{"permissions":{"deny":["write_file"]}}` document did not stop an edit, while putting the
// SESSION into `plan` mode did. The deny list is still emitted (it subtracts rights and may work on a later
// agy) but `session/set_mode` is what holds today — see agySessionMode and prepareTurnMode.

import (
	"strings"

	"github.com/beardedparrott/orchicon/internal/askmode"
	"github.com/beardedparrott/orchicon/internal/claude"
)

// agySettingDenyTool maps a platform policy name (askmode's vocabulary) onto the agy tool names that
// perform it, so `permissions.deny` in the `--settings` document can name what agy understands.
//
// ONE POLICY NAME, MANY AGY SPELLINGS, on purpose: agy's own recorded surface and the ACP documentation
// disagree about the edit tool's name (`edit_file` vs `replace_file_content`), and a deny that named only
// one of them would leave the other executable.
var agySettingDenyTool = map[string][]string{
	"bash":  {"run_command"},
	"write": {"write_file", "write_to_file", "create_file"},
	"edit":  {"edit_file", "replace_file_content", "multi_replace_file_content", "apply_patch"},
}

// agyAskModeSettings renders the `--settings` JSON for one Ask mode, or "" when the mode denies nothing.
//
// IT ALSO DENIES `ask_question` UNCONDITIONALLY. agy's native multiple-choice tool has no answer channel
// over ACP (docs/adapters/antigravity-acp.md: it is a tool_call agy renders as its own assistant text and
// then ends the turn), so the only honest routing is the platform's `orchicon_ask_user` card. Denying the
// native one turns "the model asked and the answer went nowhere" into a tool error the model can see.
func agyAskModeSettings(mode string) string {
	denied := askmode.DeniedNames(mode)
	names := make([]string, 0, len(denied)+4)
	seen := map[string]bool{}
	add := func(n string) {
		if n == "" || seen[n] {
			return
		}
		seen[n] = true
		names = append(names, n)
	}
	for _, policyName := range denied {
		for _, agyName := range agySettingDenyTool[policyName] {
			add(agyName)
		}
	}
	// agy's native ask tool, in every mode: it is not a mode boundary but a transport limitation, and it
	// is the same decision for Brainstorm, Iteration and Quick Work.
	add("ask_question")
	if len(names) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString(`{"permissions":{"deny":[`)
	for i, n := range names {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteByte('"')
		b.WriteString(n)
		b.WriteByte('"')
	}
	b.WriteString(`]}}`)
	return b.String()
}

// agySessionMode maps an Ask mode onto the agy EXECUTION MODE the live session must be put into with
// `session/set_mode`, or "" for agy's own default.
//
// WHY A SESSION MODE AT ALL, WHEN THERE IS A PERMISSION HANDLER. Because on agy 1.5.13 the handler is never
// ASKED about a file edit: the default mode writes the file with no `session/request_permission`, so a mode
// gate that only ran in the handler would refuse nothing and the operator's Brainstorm conversation would
// edit the project behind a boundary that claims to forbid it. MEASURED against the real binary: a
// `permissions.deny` list in `--settings` does NOT stop the write — that key is inert on 1.5.13 — while
// `plan` mode does. `plan` also still ALLOWS reads and still RAISES a `run_command` permission request, so
// the handler's gate keeps working for shell.
//
// THE MODE IS SET ON THE SESSION, NEVER ON ARGV. `agy --mode plan acp` is ACCEPTED (exit 0) and INERT: a
// free probe of `initialize` + `session/new` reports `modes.currentModeId = "default"` with the flag
// exactly as without it, and a LIVE turn launched that way still wrote its file. Only `session/set_mode`
// changes an ACP session's mode, which is why prepareTurnMode issues it at spawn and on every change.
//
// SO THE TWO HALVES ARE NOT INTERCHANGEABLE. The session mode is what makes "Brainstorm refrains from
// writing" TRUE rather than aspirational; the in-handler gate is what handles everything agy does ask about
// (shell, web, third-party MCP) and what refuses a tool the mode denies even in Iteration.
//
// AN UNKNOWN MODE GETS NO RESTRICTION, matching askmode.Allows and claude's gate when its mode file is
// absent: an empty mode means nobody told us the conversation's mode, and inventing `plan` would silently
// stop an Iteration conversation from writing.
func agySessionMode(mode string) string {
	if strings.TrimSpace(mode) == "" {
		return ""
	}
	if askmode.MayAct(mode) {
		// The mode may act (Iteration): agy's own default, so nothing needs setting.
		return ""
	}
	// A mode that may NOT act plans instead of acting. This is the only value that prevents the write.
	return agySessionModePlan
}

// agySessionModePlan is agy's read-only planning mode: reads work, writes never reach disk.
const agySessionModePlan = "plan"

// agySessionModeDefault is agy's own mode, spelled for `session/set_mode`. An EMPTY modeId is not a way to
// say "default" — `session/set_mode` needs a mode agy actually advertises — so releasing a restricted
// session back to normal sends this explicitly. That is what makes Iteration-after-Brainstorm work rather
// than leaving the conversation stuck in plan.
const agySessionModeDefault = "default"

// askModeDenial reports whether the turn's mode refuses this agy tool call, and the refusal to hand back.
//
// `projectDir` is the conversation's project ("" when it has none): it is only used to word the refusal
// for a path target, and an absent project must never turn a decision into an allow.
func askModeDenial(mode, tool string, input map[string]any) (bool, string) {
	if strings.TrimSpace(mode) == "" {
		// No mode stamped: no policy. The same "unknown mode allows" rule askmode.Allows applies, and the
		// same one claude's gate applies when its mode file is absent.
		return false, ""
	}

	// THE OPAQUE-MCP RULE FIRST, because it is about a NAME CLASS rather than a name, and because
	// askmode.IsOpaqueMCPTool only recognises claude's `mcp__server__tool` spelling. agy spells MCP tools
	// `mcp_<server>_<tool>`, so the platform's own server is recognised by isOrchiconMCPTool and anything
	// else under the `mcp_` prefix is opaque.
	if isMCPTool(tool) && !isOrchiconMCPTool(tool) {
		if askmode.MayAct(mode) {
			return false, ""
		}
		return true, claude.AskModeRefusal(tool, mode)
	}

	canonical := canonicalToolName(tool)
	if canonical == "" {
		// An empty name is not a call: there is nothing to refuse, and refusing "" would deny a call the
		// mode actually allows.
		return false, ""
	}
	if askmode.Allows(mode, canonical) {
		return false, ""
	}
	return true, claude.AskModeRefusal(tool, mode)
}

// askModeMayCard reports whether a surviving (not mode-refused) tool call needs the OPERATOR's decision.
//
// THE SET IS CLAUDE'S, TRANSLATED: claude's Ask profile sends Write/Edit/MultiEdit/Bash/WebSearch/WebFetch
// and every MCP tool to a card, and asks about nothing else (permissions.go AskPermissionToolNames + the
// `mcp__.*` matcher). A read, a grep or a list is not a decision anybody needs to make. agy's own
// `ask_question` is not carded either — it has no answer channel (see agyAskModeSettings), so a card for
// it would be a button that does nothing.
func askModeMayCard(tool string) bool {
	canonical := canonicalToolName(tool)
	switch canonical {
	case "write", "edit", "bash", "websearch", "webfetch":
		return true
	}
	// AN MCP TOOL IS CARDED ONLY WHEN IT IS NOT THE PLATFORM'S OWN. The platform's own tools are the
	// conversation's own surface — `orchicon_ask_user` is answered by the operator through a card of its
	// own kind, and asking them to approve the act of asking would be a card about a card.
	return isMCPTool(tool) && !isOrchiconMCPTool(tool) && canonical != "ask_question"
}
