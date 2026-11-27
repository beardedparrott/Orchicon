package antigravity

// modepin.go — AGY'S MODE IS PINNED, NOT LEFT TO WHATEVER THE OS HAPPENS TO SUPPORT.
//
// THE BUG THIS CLOSES. agy's `auto` mode is the one that can approve edits and "safe" commands without
// asking, and it needs the operating system's terminal sandbox to do it: macOS `sandbox-exec`, Linux
// user namespaces, Windows 11 24H2+. That is a THREE-WAY BEHAVIOURAL SPLIT in a single setting — the
// same tenant, the same worker, the same prompt gets a different amount of autonomy depending on which
// laptop picked up the execution — and on an OS without the sandbox agy degrades the mode silently
// rather than failing. Orchicon's permission policy (PolicyHandler → claude.DecideTool →
// workerrestrict.OutsideProject) is the ONLY thing allowed to decide whether a tool runs, and a mode
// that pre-approves anything is a second, unreviewed policy layer underneath it.
//
// SO EVERY EXECUTION IS PINNED TO `default`: ask before edits and commands, per the permission
// settings, which means every gated tool call reaches `session/request_permission` and is decided by
// OUR handler. Nothing about that is OS-dependent, which is the whole point — a run behaves the same
// on Linux, macOS and Windows.
//
// `--sandbox` IS NEVER PASSED either (acp.LaunchOptions.Sandbox stays false): opting into agy's own
// sandbox would reintroduce exactly the per-OS variance this file removes, and on a host without the
// facility it would fail at launch.
//
// MODE-GATING IS NOT UNDONE HERE. The workflow's plan/ask modes may legitimately choose a MORE
// restrictive mode; the pin is the floor for a default worker run, applied only when agy advertises
// `default` and only if it is not already the current mode. An older agy that does not advertise the
// id is left alone with a warning, never handed a request it would reject.

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/beardedparrott/orchicon/internal/antigravity/acp"
)

// agyPinnedMode is the mode every Orchicon worker execution runs agy in. See the file comment for
// why it is `default` and not `auto`.
const agyPinnedMode = "default"

// modeOption is one entry of `modes.availableModes`.
type modeOption struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// sessionModes is the `modes` object of a session/new|load|resume result.
type sessionModes struct {
	CurrentModeID  string       `json:"currentModeId"`
	AvailableModes []modeOption `json:"availableModes"`
}

// parseSessionModes decodes the raw `modes` blob. An absent or undecodable blob yields the zero
// value, which callers treat as "agy advertised nothing".
func parseSessionModes(raw json.RawMessage) sessionModes {
	var m sessionModes
	if len(raw) == 0 {
		return m
	}
	// A decode failure is not fatal: agy adds fields between releases and a mode pin is not worth
	// failing a run over. The caller's `advertised` check handles the empty result.
	_ = json.Unmarshal(raw, &m)
	return m
}

// advertisesMode reports whether agy listed id in `modes.availableModes`.
func (m sessionModes) advertisesMode(id string) bool {
	for _, a := range m.AvailableModes {
		if strings.EqualFold(strings.TrimSpace(a.ID), id) {
			return true
		}
	}
	return false
}

// pinMode sends `session/set_mode` for agyPinnedMode when agy advertises it and is not already in it.
//
// IT RETURNS AN ERROR ONLY WHEN AGY REJECTED AN ADVERTISED MODE, which would mean the run is about to
// proceed under a mode nobody chose — the ambiguity this function exists to remove. An agy that does
// not advertise `default` is a WARNING and not an error: the run continues in agy's own mode, which is
// the pre-existing behaviour for such a build.
func (s *session) pinMode(ctx context.Context, conv *acp.Session, state *acp.SessionState) error {
	var raw json.RawMessage
	if state != nil {
		raw = state.Modes
	}
	modes := parseSessionModes(raw)

	if !modes.advertisesMode(agyPinnedMode) {
		s.b.log.Warn("antigravity: agy does not advertise the pinned mode, leaving its own default in place",
			"execution", s.execID, "wanted_mode", agyPinnedMode,
			"current_mode", modes.CurrentModeID, "advertised", len(modes.AvailableModes))
		return nil
	}
	if strings.EqualFold(strings.TrimSpace(modes.CurrentModeID), agyPinnedMode) {
		// Already there. Sending the call anyway would be harmless but noisy, and it would make the
		// mode indistinguishable from a mode this run deliberately CHOSE.
		s.b.log.Debug("antigravity: agy already in the pinned mode", "execution", s.execID, "mode", agyPinnedMode)
		return nil
	}
	if err := conv.SetMode(ctx, agyPinnedMode); err != nil {
		return err
	}
	s.b.log.Info("antigravity: pinned agy mode", "execution", s.execID, "mode", agyPinnedMode,
		"previous", modes.CurrentModeID)
	return nil
}
