package claude

// modegate.go — the claude transport's ENFORCEMENT of the Ask mode boundary.

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

// askModeStateDirName is the platform-owned directory, a SIBLING of every conversation's ask
// directory, that holds the mode files.
const askModeStateDirName = ".state"

// AskModeState is the file's contents: the mode, the tools it denies, and whether the mode MAY ACT.
type AskModeState struct {
	Mode   string   `json:"mode"`
	Denied []string `json:"denied"`
	MayAct bool     `json:"may_act"`
}

// askModeFilePath returns the mode file for one conversation.
func askModeFilePath(askRoot, convID string) string {
	return filepath.Join(askRoot, askModeStateDirName, convID+".mode.json")
}

// writeAskModeFile records the turn's mode, or CLEARS it when the mode denies nothing AND may act.
func writeAskModeFile(path, mode string) error {
	denied := askmode.DeniedNames(mode)
	mayAct := askmode.MayAct(mode)
	if len(denied) == 0 && mayAct {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	b, err := json.Marshal(AskModeState{Mode: mode, Denied: denied, MayAct: mayAct})
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

// readAskModeFile loads a mode file.
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
		// A garbled file is NOT read as an empty policy by accident: the caller treats ok=false as "no
		// policy", which is the same outcome.
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

	// MCP TOOLS, whose names claude prefixes: `mcp__<server>__<toolName>`.
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
// TWO KINDS OF DENIAL, from the ONE table (internal/askmode), and the OPAQUE-MCP
// rule comes first because it is the one a bare-name lookup cannot express:
//
//  1. an OPAQUE MCP tool (`mcp__<server>__<tool>`, server != the platform's own
//     orchicon sidecar) is an ACTION to a table that cannot classify it, so it is
//     refused by any mode that MAY NOT ACT — read from the flag the turn's writer
//     recorded (askmode.MayAct), never re-derived here. This is what makes a
//     Brainstorm conversation unable to call an operator's MCP server.
//  2. the mode's classic denials, via the translated bare name.
//
// BOTH ARE READ FROM THE SAME FILE, written from the same table, and the WORDING
// is shared — mirroring askorchicon's native refusal so a model sees the same
// boundary whichever adapter it runs on. It states that the PLATFORM refused it,
// that the model cannot switch its own mode, and the one action that resolves it —
// the user switching the mode.
func askModeDenial(path, tool string) (bool, string) {
	st, ok := readAskModeFile(path)
	if !ok {
		return false, ""
	}

	if askmode.IsOpaqueMCPTool(tool) {
		if st.MayAct {
			return false, ""
		}
		return true, askModeRefusal(tool, st.Mode)
	}

	name := claudeToolToPolicyName(tool)
	if name == "" {
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
	return true, askModeRefusal(tool, st.Mode)
}

// askModeRefusal builds the refusal both denial kinds hand back.
func askModeRefusal(tool, mode string) string {
	p, _ := askmode.PolicyFor(mode)
	why := strings.TrimSpace(p.Why)
	if why == "" {
		why = "this mode does not do that kind of work"
	}
	switchTo := strings.TrimSpace(p.SwitchTo)
	if switchTo == "" {
		switchTo = askmode.Iteration
	}
	return "REFUSED BY THE PLATFORM: " + tool + " is not available in " + mode +
		" mode, so it was NOT executed. " + why +
		"\n\nYou cannot override this, and you cannot switch your own mode — only the user can, from the mode " +
		"selector on this conversation. Say plainly that this is a " + switchTo + " job, name the mode, and ASK " +
		"THE USER TO SWITCH TO " + strings.ToUpper(switchTo) + ". Do not attempt the call again, and do not work " +
		"around it with another tool."
}

// isAskStatePath reports whether a write target is inside the platform's Ask state directory (the
// mode files).
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
