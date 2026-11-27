package askorchicon

// antigravity_consent_test.go — the antigravity (agy) tool-name aliases in the consent layer.
//
// WHY THIS IS A SECURITY TEST, NOT A COSMETIC ONE. The antigravity adapter emits the RAW agy tool name on
// its `permission` bus event (that is what the AC pins: `Tool:"run_command"`), and every shell rule in this
// file's decision chain keys on `isBashAsk`: the never-allow binary class, `commandInvokesScopedBinary`
// gating, the path-extraction arm and the card's intent verb. When `isBashAsk("run_command")` was false,
// `rm -rf /` arriving through agy skipped ALL of them and reached the operator as an ordinary card — the
// single worst place a "silently not applied" rule can land.
//
// The same reasoning holds for the web aliases: a web target that is not recognised as one becomes a
// FABRICATED filesystem path (see the isWebTool comment), which then claims to be inside the project and
// becomes the key a grant is stored under.

import (
	"testing"

	"github.com/beardedparrott/orchicon/internal/scheduler"
)

// TestConsentRefusesNeverAllowCommandThroughAntigravity is the guard: every destructive command arriving
// with agy's spelling gets the SAME verdict claude's `Bash` spelling gets.
//
// IT IS A DIFFERENTIAL TEST ON PURPOSE. The exact rung that fires differs by command — a never-allow binary
// (`sudo …`, `mkfs…`) is refused by binaryClassRefusal, while `rm -rf /` is refused by the protected-path
// rung — and pinning "reject" for all of them would make the test assert a rung rather than the ALIAS. What
// must never happen is the two spellings disagreeing, so each command is decided twice, once per spelling.
func TestConsentRefusesNeverAllowCommandThroughAntigravity(t *testing.T) {
	// `want` is the verdict for the NEVER-ALLOW class only; "" means "whatever claude's spelling got", because
	// that command's fate is decided by a different rung and the alias is what this test is about.
	cases := []struct{ cmd, want string }{
		// a never-allow binary: refused before any path reasoning.
		{"sudo rm -rf /", "reject"},
		{"mkfs.ext4 /dev/sda1", "reject"},
		{"dd if=/dev/zero of=/dev/sda", "reject"},
		{"rm -rf /", ""},
	}
	for _, tc := range cases {
		bashResp, bashAsk, bashRef := decideBashSpelling(t, "bash", tc.cmd)
		agyResp, agyAsk, agyRef := decideBashSpelling(t, "run_command", tc.cmd)

		if agyResp != bashResp || (agyAsk == nil) != (bashAsk == nil) || (agyRef == "") != (bashRef == "") {
			t.Errorf("decide(%q) via agy's `run_command` disagrees with claude's `bash`:\n"+
				"  bash:        resp=%q ask=%v refusal=%q\n"+
				"  run_command: resp=%q ask=%v refusal=%q\n"+
				"THE ALIAS IS THE GUARD. Every shell rule in the decision chain keys on isBashAsk, so a spelling\n"+
				"that is not recognised skips the never-allow class, the scoped-binary gate and the path arm.", tc.cmd,
				bashResp, bashAsk != nil, bashRef, agyResp, agyAsk != nil, agyRef)
		}
		if tc.want != "" && agyResp != tc.want {
			t.Errorf("decide(run_command %q) = %q, want %q", tc.cmd, agyResp, tc.want)
		}
		if tc.cmd != "rm -rf /" && agyAsk != nil {
			t.Errorf("decide(run_command %q) raised a CARD — a never-allow class must never reach the operator as an ask", tc.cmd)
		}
	}
}

// decideBashSpelling runs one decision with an explicit tool name.
func decideBashSpelling(t *testing.T, tool, cmd string) (string, *pendingAsk, string) {
	t.Helper()
	svc := testConsentService()
	isolatedPolicy(t, "")
	ct := newTestConsentTurn(svc, t.TempDir(), true, nil)
	return ct.decide(tenantCtx(), "sid-1", scheduler.SessionEvent{
		Kind:         "permission",
		PermissionID: "ask-1",
		Tool:         tool,
		Command:      cmd,
		InputJSON:    `{"CommandLine":"` + cmd + `"}`,
	})
}

// TestBashAndWebAskAliasesDoc pins the predicate itself, so a future rename cannot quietly drop one spelling.
func TestBashAndWebAskAliases(t *testing.T) {
	for _, name := range []string{"bash", "shell", "sh", "run_command"} {
		if !isBashAsk(name) {
			t.Errorf("isBashAsk(%q) = false — every shell rule in the decision chain skips this tool", name)
		}
	}
	for _, name := range []string{"webfetch", "websearch", "web_fetch", "web_search", "read_url_content", "search_web"} {
		if !isWebTool(name) {
			t.Errorf("isWebTool(%q) = false — a web target becomes a fabricated filesystem path", name)
		}
	}
	if isBashAsk("write_to_file") {
		t.Error("isBashAsk(write_to_file) = true — a write would be judged as a shell command")
	}
	if isWebTool("write_to_file") {
		t.Error("isWebTool(write_to_file) = true")
	}
}

// TestToolIntentVerbCoversAntigravitySpellings documents the card's verb for agy's names: an in-place edit
// that says "use the tool" tells the operator nothing.
func TestToolIntentVerbCoversAntigravitySpellings(t *testing.T) {
	cases := map[string]string{
		"run_command":          "run a shell command",
		"replace_file_content": "modify",
		"write_to_file":        "modify",
		"view_file":            "read",
		"read_url_content":     "read",
	}
	for tool, want := range cases {
		if got := toolIntentVerb(tool); got != want {
			t.Errorf("toolIntentVerb(%q) = %q, want %q", tool, got, want)
		}
	}
}
