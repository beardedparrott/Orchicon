package claude

// modegate.go — the claude transport's ENFORCEMENT of the Ask mode boundary.
//
// THE POLICY IS NOT HERE. It lives in internal/askmode, which imports nothing
// and says nothing about HOW a mode is refused ("the wording is the caller's").
// This file is the claude half: carrying the turn's mode to the tool boundary and
// turning a denial into a MESSAGE the model can relay.
//
// # Why a file, when the native adapter needs nothing
//
// The native transport IS its own tool loop — it lists the Ask tools and executes
// them — so askorchicon's provider applies askmode at both points and
// NativeBridge.RestrictChatTools is a documented no-op. claude is the opposite
// case: it drives its own external process, and the only point the platform can
// interpose is the PreToolUse hook.
//
// The hook is a FRESH PROCESS per tool call (claude spawns the command from the
// settings document for each PreToolUse event), so it re-reads its inputs every
// time. That is what makes a FILE the right carrier: the adapter rewrites it per
// turn, and the very next tool call sees the new mode. A child's ENVIRONMENT
// could not do this — it is fixed at spawn, so a mid-conversation mode switch
// would need a session restart.
//
// # The refusal is a DENY, not an ask
//
// A mode boundary is not the operator's per-call decision, it is a mode rule, and
// only the user can change it (the mode selector). Offering it as a consent card
// would tell the operator "you may approve this" about something the mode has
// already refused — the same lie the guard's never-allow class avoids. So the
// hook returns deny with a message that names the mode to switch to.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"github.com/beardedparrott/orchicon/internal/askmode"
)

// AskModeFileEnv names the per-conversation mode file this session's hook reads.
// Set at spawn; its CONTENTS change per turn, which is the whole mechanism.
const AskModeFileEnv = "ORCHICON_CLAUDE_ASK_MODE_FILE"

// askModeStateDirName is the platform-owned directory, a SIBLING of every
// conversation's ask directory, that holds the mode files.
//
// It is deliberately NOT inside the conversation's own directory. The mode file
// is the enforcement, so a session that could rewrite it could lift its own
// restriction — and the conversation's directory is exactly the place it can
// write. As a sibling it is reachable only by traversing out (`../.state`, which
// the shim refuses) or by an absolute path (refused as out of scope), and the
// hook refuses it a third time (see isAskStatePath), because claude's Write tool
// does not go through the shim at all.
const askModeStateDirName = ".state"

// AskModeState is the file's contents: the mode, and the tools it denies.
//
// Denied is carried EXPLICITLY rather than recomputed from the mode, so the hook
// does not have to agree with the writer about which policy version is current —
// the file states the decision that was actually taken for the turn.
type AskModeState struct {
	Mode   string   `json:"mode"`
	Denied []string `json:"denied"`
}

// askModeFilePath returns the mode file for one conversation.
func askModeFilePath(askRoot, convID string) string {
	return filepath.Join(askRoot, askModeStateDirName, convID+".mode.json")
}

// writeAskModeFile records the turn's mode, or CLEARS it when the mode denies
// nothing.
//
// A mode with no policy (empty, or a value written by an older build) allows
// everything — see askmode.Allows — so the file is removed rather than written
// with an empty denial list. Absence and "denies nothing" then mean the same
// thing to the reader, and a stale file cannot outlive the mode it described.
//
// Written atomically (temp + rename) because the hook may read at any moment: a
// partially written file would parse as a garbled mode, and the safe reading of a
// garbled file is "no policy", i.e. the boundary silently drops.
func writeAskModeFile(path, mode string) error {
	denied := askmode.DeniedNames(mode)
	if len(denied) == 0 {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	b, err := json.Marshal(AskModeState{Mode: mode, Denied: denied})
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// readAskModeFile loads a mode file. A missing or unreadable file yields ok=false
// — "no mode policy for this turn", which is the documented meaning of an
// unstamped turn rather than an error.
func readAskModeFile(path string) (AskModeState, bool) {
	if strings.TrimSpace(path) == "" {
		return AskModeState{}, false
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return AskModeState{}, false
	}
	var st AskModeState
	if err := json.Unmarshal(b, &st); err != nil {
		// A garbled file is NOT read as an empty policy by accident: the caller
		// treats ok=false as "no policy", which is the same outcome, and the
		// writer's atomic rename is what keeps this from happening in practice.
		return AskModeState{}, false
	}
	return st, true
}

// claudeToolToPolicyName maps claude's tool vocabulary onto the mode policy's.
//
// THIS MAPPING IS THE BOUNDARY. askmode names tools in the native/Ask registry
// spelling (`write`, `edit`, `bash`, `create_work_item`, …) because that is the
// registry the native adapter lists from. claude names the same ACTIONS
// differently and splits some across tools, so a denial that is not translated
// here is a boundary that silently does not hold — the failure mode this whole
// file exists to prevent.
//
//	claude                 policy
//	Write                  write
//	Edit, MultiEdit,       edit      (all three WRITE; the mode boundary is about
//	NotebookEdit                     the action, not the file type)
//	Bash                   bash
//
// Everything else returns "" — not a policy-controlled tool. In particular the
// PLANNER tools (create_work_item, schedule_work_item, …) do not appear: claude
// has no such tool unless an Orchicon MCP server is configured, so there is
// nothing here to deny. If that changes, this map is where the pairing goes.
func claudeToolToPolicyName(tool string) string {
	t := strings.ToLower(strings.TrimSpace(tool))

	// MCP TOOLS, whose names claude prefixes: `mcp__<server>__<toolName>`. The
	// policy names the tool ALONE, so a boundary written as `create_work_item`
	// never matches `mcp__orchicon__create_work_item` — the mode gate would be
	// inert for precisely the tools it was written to govern, and silently so.
	// Naming verified against the real CLI's init line: 87 tools named
	// mcp__orchicon__* once the sidecar is registered (mcpconfig.go).
	if rest, ok := strings.CutPrefix(t, "mcp__"); ok {
		if i := strings.Index(rest, "__"); i >= 0 {
			rest = rest[i+2:]
		}
		return strings.TrimSpace(rest)
	}

	switch t {
	case "write":
		return "write"
	case "edit", "multiedit", "notebookedit":
		return "edit"
	case "bash":
		return "bash"
	default:
		return ""
	}
}

// askModeDenial reports whether the turn's mode refuses this tool call, and the
// refusal to hand back.
//
// The DECISION is askmode's (via the file's Denied list, which was askmode's
// answer at write time); only the WORDING is here, mirroring askorchicon's native
// refusal so a model sees the same boundary whichever adapter it runs on. It
// states that the PLATFORM refused it, that the model cannot switch its own mode,
// and the one action that resolves it — the user switching the mode.
func askModeDenial(path, tool string) (bool, string) {
	name := claudeToolToPolicyName(tool)
	if name == "" {
		return false, ""
	}
	st, ok := readAskModeFile(path)
	if !ok {
		return false, ""
	}
	denied := false
	for _, d := range st.Denied {
		if d == name {
			denied = true
			break
		}
	}
	if !denied {
		return false, ""
	}

	p, _ := askmode.PolicyFor(st.Mode)
	why := strings.TrimSpace(p.Why)
	if why == "" {
		why = "this mode does not do that kind of work"
	}
	switchTo := strings.TrimSpace(p.SwitchTo)
	if switchTo == "" {
		switchTo = askmode.Iteration
	}

	return true, "REFUSED BY THE PLATFORM: " + tool + " is not available in " + st.Mode +
		" mode, so it was NOT executed. " + why +
		"\n\nYou cannot override this, and you cannot switch your own mode — only the user can, from the mode " +
		"selector on this conversation. Say plainly that this is a " + switchTo + " job, name the mode, and ASK " +
		"THE USER TO SWITCH TO " + strings.ToUpper(switchTo) + ". Do not attempt the call again, and do not work " +
		"around it with another tool."
}

// isAskStatePath reports whether a write target is inside the platform's Ask
// state directory (the mode files).
//
// THE ANTI-TAMPER RULE, and the reason it is at the hook rather than only in the
// shim: claude's Write/Edit tools act in-process, so the OS-level shim never sees
// them, and the file these guard is the enforcement itself. A session that
// rewrote its own mode file would be overriding the mode boundary — exactly what
// the refusal text tells the model it cannot do.
func isAskStatePath(target, modeFilePath string) bool {
	if strings.TrimSpace(target) == "" || strings.TrimSpace(modeFilePath) == "" {
		return false
	}
	stateDir := filepath.Dir(modeFilePath)
	abs, err := filepath.Abs(target)
	if err != nil {
		return false
	}
	stateAbs, err := filepath.Abs(stateDir)
	if err != nil {
		return false
	}
	if abs == stateAbs {
		return true
	}
	return strings.HasPrefix(abs, stateAbs+string(os.PathSeparator))
}
