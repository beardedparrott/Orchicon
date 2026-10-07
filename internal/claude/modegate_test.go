package claude

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/askmode"
)

// The mode vocabulary differs between the policy and claude's tool surface, and
// an untranslated denial is a boundary that silently does not hold. Every row is
// pinned.
func TestClaudeToolToPolicyName(t *testing.T) {
	cases := map[string]string{
		"Write":        "write",
		"write":        "write",
		"Edit":         "edit",
		"MultiEdit":    "edit",
		"NotebookEdit": "edit",
		"Bash":         "bash",
		// Not policy-controlled: reads observe rather than act, and claude has no
		// Orchicon planner tools at all unless an MCP server provides them.
		"Read":             "",
		"Glob":             "",
		"Grep":             "",
		"Task":             "",
		"create_work_item": "",
		"":                 "",
	}
	for in, want := range cases {
		if got := claudeToolToPolicyName(in); got != want {
			t.Errorf("claudeToolToPolicyName(%q) = %q, want %q", in, got, want)
		}
	}
}

// Write/Edit/MultiEdit/NotebookEdit are all the SAME action to a mode boundary.
// MultiEdit and NotebookEdit are the easy ones to forget, and forgetting them
// leaves a brainstorm conversation able to write through two other tools.
func TestModeDeniesEveryWriteShapedTool(t *testing.T) {
	path := filepath.Join(t.TempDir(), "conv.mode.json")
	if err := writeAskModeFile(path, askmode.Brainstorm); err != nil {
		t.Fatalf("writeAskModeFile: %v", err)
	}
	for _, tool := range []string{"Write", "Edit", "MultiEdit", "NotebookEdit", "Bash"} {
		denied, msg := askModeDenial(path, tool)
		if !denied {
			t.Errorf("%s was NOT denied in %s mode — the boundary has a hole", tool, askmode.Brainstorm)
			continue
		}
		if !strings.Contains(msg, "REFUSED BY THE PLATFORM") {
			t.Errorf("%s refusal does not say the platform refused it: %q", tool, msg)
		}
		if !strings.Contains(msg, askmode.Iteration) {
			t.Errorf("%s refusal does not name the mode to switch to: %q", tool, msg)
		}
	}
	// A read is never mode-denied, in any mode.
	if denied, _ := askModeDenial(path, "Read"); denied {
		t.Error("Read was denied in brainstorm mode; reads observe rather than act")
	}
}

// The gate is POLICY-driven, not "writes are always refused": iteration denies
// the PLANNER tools, so a write is legitimate there. A test asserting only the
// brainstorm half would pass even if the implementation hard-denied writes.
func TestIterationModeDoesNotDenyClaudesTools(t *testing.T) {
	path := filepath.Join(t.TempDir(), "conv.mode.json")
	if err := writeAskModeFile(path, askmode.Iteration); err != nil {
		t.Fatalf("writeAskModeFile: %v", err)
	}
	for _, tool := range []string{"Write", "Edit", "Bash"} {
		if denied, msg := askModeDenial(path, tool); denied {
			t.Errorf("%s was denied in iteration mode: %q", tool, msg)
		}
	}
	// ...while the file DOES record iteration's own denial set, so the policy is
	// being read rather than ignored.
	st, ok := readAskModeFile(path)
	if !ok {
		t.Fatal("the iteration mode file is missing")
	}
	if st.Mode != askmode.Iteration || len(st.Denied) == 0 {
		t.Fatalf("mode file = %+v, want the iteration policy recorded", st)
	}
}

// A mode with no policy allows everything (askmode.Allows), and absence and
// "denies nothing" must mean the same thing to the reader — otherwise a stale
// file outlives the mode that wrote it.
func TestWriteAskModeFileClearsAPolicyLessMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "conv.mode.json")
	if err := writeAskModeFile(path, askmode.Brainstorm); err != nil {
		t.Fatalf("writeAskModeFile: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("the brainstorm mode file was not written: %v", err)
	}
	if err := writeAskModeFile(path, ""); err != nil {
		t.Fatalf("writeAskModeFile(empty): %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("an empty mode must CLEAR the file, or a stale boundary outlives its mode")
	}
	if denied, _ := askModeDenial(path, "Write"); denied {
		t.Error("Write was denied with no mode file present")
	}
}

// The anti-tamper rule: the mode file IS the enforcement, so a session must not
// be able to write it. claude's Write/Edit act in-process, so the OS shim never
// sees them — this check is the only thing standing between the model and its own
// restriction.
func TestIsAskStatePath(t *testing.T) {
	modeFile := "/tmp/orchicon-ask/.state/conv-1.mode.json"
	cases := map[string]bool{
		"/tmp/orchicon-ask/.state/conv-1.mode.json":    true,
		"/tmp/orchicon-ask/.state/conv-2.mode.json":    true,
		"/tmp/orchicon-ask/.state/":                    true,
		"/tmp/orchicon-ask/.state":                     true,
		"/tmp/orchicon-ask/conv-1/notes.txt":           false,
		"/tmp/orchicon-ask/.stateful/conv-1.mode.json": false, // prefix, not a child
		"": false,
	}
	for target, want := range cases {
		if got := isAskStatePath(target, modeFile); got != want {
			t.Errorf("isAskStatePath(%q) = %v, want %v", target, got, want)
		}
	}
}

// END TO END through the hook's wire protocol: in brainstorm mode a Write must be
// refused with a DENY, not offered as a consent card. A card here would tell the
// operator they may approve something the mode has already refused.
func TestRunHookDeniesAModeBlockedToolInTheAskProfile(t *testing.T) {
	askDir := t.TempDir()
	modeFile := filepath.Join(askDir, "conv.mode.json")
	if err := writeAskModeFile(modeFile, askmode.Brainstorm); err != nil {
		t.Fatalf("writeAskModeFile: %v", err)
	}
	env := map[string]string{
		HookProfileEnv: ProfileAskEnvValue,
		AskDirEnv:      askDir,
		AskModeFileEnv: modeFile,
	}
	payload := `{"hook_event_name":"PreToolUse","tool_name":"Write","tool_input":{"file_path":"` +
		filepath.Join(askDir, "out.txt") + `"}}`

	var out bytes.Buffer
	RunHook(strings.NewReader(payload), &out, func(k string) string { return env[k] })

	var doc hookOutput
	if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
		t.Fatalf("hook output is not valid JSON (%q): %v", out.String(), err)
	}
	if got := doc.HookSpecificOutput.PermissionDecision; got != DecisionDeny {
		t.Fatalf("permissionDecision = %q, want %q — a mode-blocked tool must be a refusal, never a card", got, DecisionDeny)
	}
	if !strings.Contains(doc.HookSpecificOutput.PermissionDecisionReason, "REFUSED BY THE PLATFORM") {
		t.Errorf("reason does not read as a platform refusal: %q", doc.HookSpecificOutput.PermissionDecisionReason)
	}
}

// NO MODE FILE = no mode claim, and a write is still the operator's decision — the regression guard
// for the pre-mode behaviour.
//
// THE VERDICT IS "ASK", AND THE WIRE CARRIES NOTHING: the mode file decides whether the MODE refuses a
// call; an ask is not a refusal, and the hook delivers an ask by abstaining so the permission system
// raises the host prompt (see TestRunHookAbstainsSoThePermissionSystemCanAsk). So the assertion is on
// the DECISION — the mode file was consulted and produced no denial — and on the ABSENCE of any wire
// verdict, which is the half that was broken.
func TestRunHookWithoutAModeFileStillAsks(t *testing.T) {
	env := map[string]string{
		HookProfileEnv: ProfileAskEnvValue,
		AskDirEnv:      t.TempDir(),
		// AskModeFileEnv deliberately unset.
	}
	payload := `{"hook_event_name":"PreToolUse","tool_name":"Write","tool_input":{"file_path":"/tmp/x"}}`

	// The mode layer itself: no file, no denial.
	if denied, reason := askModeDenial("", "Write"); denied {
		t.Fatalf("with no mode file the mode gate denied a write (%q) — there is no mode boundary to enforce", reason)
	}

	var out bytes.Buffer
	RunHook(strings.NewReader(payload), &out, func(k string) string { return env[k] })

	if got := strings.TrimSpace(out.String()); got != "" {
		t.Fatalf("the hook emitted %q for an ask, want no output — a write with no mode boundary must reach "+
			"the operator as a CARD (via permissions.ask), not as a wire verdict the CLI turns into an error", got)
	}
}

// The tamper rule through the wire: a write into the platform's Ask state is
// refused even though the target is a plain path the Ask profile would otherwise
// be happy to ask about.
func TestRunHookRefusesWritesToTheAskState(t *testing.T) {
	askDir := t.TempDir()
	modeFile := filepath.Join(askDir, askModeStateDirName, "conv-1.mode.json")
	env := map[string]string{
		HookProfileEnv: ProfileAskEnvValue,
		AskDirEnv:      askDir,
		AskModeFileEnv: modeFile,
	}
	payload := `{"hook_event_name":"PreToolUse","tool_name":"Write","tool_input":{"file_path":"` + modeFile + `"}}`

	var out bytes.Buffer
	RunHook(strings.NewReader(payload), &out, func(k string) string { return env[k] })

	var doc hookOutput
	if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
		t.Fatalf("hook output is not valid JSON: %v", err)
	}
	if got := doc.HookSpecificOutput.PermissionDecision; got != DecisionDeny {
		t.Fatalf("permissionDecision = %q, want deny — the session rewrote its own mode boundary", got)
	}
}

// THE ADAPTER HALF: the mode file is written per TURN from the context the
// platform stamped, so a mid-conversation switch is enforced on the next turn,
// and clearing it removes the boundary. This is the end the hook reads.
func TestSendTurnMessageRecordsTheTurnsMode(t *testing.T) {
	h, _ := askHarness(t)
	s := h.b.ensureAskSession("conv-mode")
	if s.modeFile == "" {
		t.Fatal("the ask session has no mode file path")
	}

	// Brainstorm: the work tools are refused, so the file records the boundary.
	ctx := askmode.WithMode(context.Background(), askmode.Brainstorm)
	if err := h.b.SendTurnMessage(ctx, "conv-mode", "sid-1", "", "claude/anthropic/claude-sonnet-4", "hi"); err != nil {
		t.Fatalf("SendTurnMessage: %v", err)
	}
	if denied, _ := askModeDenial(s.modeFile, "Write"); !denied {
		t.Fatal("brainstorm's boundary was not recorded — a write would have been allowed")
	}

	// Switch to iteration: the boundary must change on the NEXT turn, with no
	// session restart (which is why the carrier is a file and not the env).
	ctx = askmode.WithMode(context.Background(), askmode.Iteration)
	if err := h.b.SendTurnMessage(ctx, "conv-mode", "sid-1", "", "claude/anthropic/claude-sonnet-4", "more"); err != nil {
		t.Fatalf("SendTurnMessage (iteration): %v", err)
	}
	if denied, msg := askModeDenial(s.modeFile, "Write"); denied {
		t.Fatalf("the mode switch did not take effect on the next turn: %q", msg)
	}
}
