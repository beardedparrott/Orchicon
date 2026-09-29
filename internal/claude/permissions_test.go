package claude

// permissions_test.go — T1: the launch shape. NO live claude session, no
// Anthropic spend: every assertion is over the argv the adapter would emit and
// the settings JSON document inside it.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/neverallow"
	"github.com/beardedparrott/orchicon/internal/scheduler"
	"github.com/beardedparrott/orchicon/internal/workerrestrict"
)

// skipPermissions matches every spelling of a permission bypass the CLI accepts.
var skipPermissions = regexp.MustCompile(`(?i)skip[-_]?permissions`)

func settingsDoc(t *testing.T, args []string) map[string]any {
	t.Helper()
	for i, a := range args {
		if a == "--settings" {
			if i+1 >= len(args) {
				t.Fatal("--settings has no value")
			}
			var doc map[string]any
			if err := json.Unmarshal([]byte(args[i+1]), &doc); err != nil {
				t.Fatalf("the --settings value is not valid JSON: %v", err)
			}
			return doc
		}
	}
	t.Fatal("the launch argv carries no --settings document")
	return nil
}

// THE HEADLINE PIN. A blanket bypass launch would silently discard every
// restriction this package exists to express, so the test must FAIL the moment
// one is introduced — in the argv, in the settings document, or as the value of
// --permission-mode.
func TestNoBypassPermissionFlagIsEverEmitted(t *testing.T) {
	project := t.TempDir()
	args, err := PermissionArgs(PermissionOptions{
		ProjectDir: project,
		HookBinary: "/usr/local/bin/orchicon",
	})
	if err != nil {
		t.Fatalf("PermissionArgs: %v", err)
	}

	joined := strings.Join(args, " ")
	for _, tok := range bypassLaunchFlags {
		if strings.Contains(strings.ToLower(joined), strings.ToLower(tok)) {
			t.Fatalf("the launch argv contains the bypass flag %q: %v", tok, args)
		}
	}
	if skipPermissions.MatchString(joined) {
		t.Fatalf("the launch argv carries a skip-permissions spelling: %v", args)
	}

	// --permission-mode must be present and must be `default` (NOT acceptEdits,
	// NOT bypassPermissions): an unanswerable ask in -p mode is a refusal, which
	// is the honest worker semantic.
	mode := ""
	for i, a := range args {
		if a == "--permission-mode" && i+1 < len(args) {
			mode = args[i+1]
		}
	}
	if mode != "default" {
		t.Fatalf("--permission-mode = %q, want %q", mode, "default")
	}
	if mode == bypassPermissionMode {
		t.Fatal("the launch uses the bypass permission mode")
	}

	// And the settings layer pins the bypass mode OFF, so a later
	// `--permission-mode bypassPermissions` would be refused by claude itself.
	doc := settingsDoc(t, args)
	perm, _ := doc["permissions"].(map[string]any)
	if got := perm["disableBypassPermissionsMode"]; got != "disable" {
		t.Errorf("permissions.disableBypassPermissionsMode = %#v, want %q", got, "disable")
	}
	if got := perm["defaultMode"]; got != "default" {
		t.Errorf("permissions.defaultMode = %#v, want %q", got, "default")
	}
}

func TestSettingsExpressThePlatformRestrictions(t *testing.T) {
	project := t.TempDir()
	args, err := PermissionArgs(PermissionOptions{
		ProjectDir:  project,
		WorktreeDir: filepath.Join(project, "wt"),
		HookBinary:  "/usr/local/bin/orchicon",
	})
	if err != nil {
		t.Fatalf("PermissionArgs: %v", err)
	}
	doc := settingsDoc(t, args)
	settings := ""
	for i, a := range args {
		if a == "--settings" {
			settings = args[i+1]
		}
	}
	perm, _ := doc["permissions"].(map[string]any)

	// The never-allow class is CONSUMED from internal/neverallow, not restated.
	deny := stringSet(perm["deny"])
	for _, b := range neverallow.Shimmed() {
		want := "Bash(" + b + ":*)"
		if !deny[want] {
			t.Errorf("permissions.deny is missing %q — the launch does not consume the shared never-allow class", want)
		}
	}

	// The built-in subagent tool (opencode's `task` deny) is denied by name.
	for _, n := range workerrestrict.SubagentToolNames {
		if !deny[n] {
			t.Errorf("permissions.deny is missing the subagent tool %q", n)
		}
	}
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "--disallowedTools") || !strings.Contains(joined, workerrestrict.SubagentToolNames[0]) {
		t.Errorf("--disallowedTools does not name the subagent tool: %v", args)
	}

	// The carve-outs are the SAME two the opencode worker profile grants.
	dirs := stringSet(perm["additionalDirectories"])
	if !dirs[workerrestrict.ScratchDir] {
		t.Errorf("additionalDirectories lacks the scratch carve-out %q: %v", workerrestrict.ScratchDir, perm["additionalDirectories"])
	}
	if !dirs[filepath.Join(project, ".orchicon")] {
		t.Errorf("additionalDirectories lacks the run-metadata carve-out: %v", perm["additionalDirectories"])
	}

	// The PreToolUse hook is the authority, and it invokes the product binary.
	if got := SettingsHookBinary(settings); got != "/usr/local/bin/orchicon" {
		t.Errorf("hook binary = %q, want /usr/local/bin/orchicon", got)
	}
	hooks, _ := doc["hooks"].(map[string]any)
	pre, _ := hooks["PreToolUse"].([]any)
	if len(pre) != 1 {
		t.Fatalf("hooks.PreToolUse = %#v, want one matcher entry", hooks["PreToolUse"])
	}
	entry, _ := pre[0].(map[string]any)
	cmds, _ := entry["hooks"].([]any)
	if len(cmds) != 1 {
		t.Fatalf("the PreToolUse entry has no command hook: %#v", entry)
	}
	cmd, _ := cmds[0].(map[string]any)
	if got, _ := cmd["command"].(string); !strings.HasSuffix(got, " claude-hook") {
		t.Errorf("hook command = %q, want it to invoke `claude-hook`", got)
	}
	if got, _ := entry["matcher"].(string); !strings.Contains(got, "Bash") || !strings.Contains(got, "Task") {
		t.Errorf("hook matcher = %q, want it to cover the command, path and subagent tools", got)
	}
}

// The operator's policy deny entries are expressed as read AND edit denies, and a
// MALFORMED policy is RETURNED as an error (never silently dropped) while the
// launch shape stays restrictive — the fail-closed behaviour
// internal/permpolicy/policy_load_failure_test.go pins, preserved at this layer.
func TestSettingsCarryThePolicyDeniesAndFailClosedOnLoadFailure(t *testing.T) {
	project := t.TempDir()

	good := filepath.Join(t.TempDir(), "policy.yaml")
	if err := os.WriteFile(good, []byte("deny:\n  - \"**/.ssh/**\"\naccept: []\n"), 0o600); err != nil {
		t.Fatalf("write policy: %v", err)
	}
	settings, err := BuildSettings(PermissionOptions{ProjectDir: project, HookBinary: "orchicon", PolicyPath: good})
	if err != nil {
		t.Fatalf("a valid policy must not error: %v", err)
	}
	deny := stringSet(settingsDoc(t, []string{"--settings", settings})["permissions"].(map[string]any)["deny"])
	if !deny["Read(**/.ssh/**)"] || !deny["Edit(**/.ssh/**)"] {
		t.Errorf("policy deny entries are not expressed as Read/Edit rules: %v", deny)
	}

	broken := filepath.Join(t.TempDir(), "broken.yaml")
	if err := os.WriteFile(broken, []byte("deny: [oops\naccept: {\n"), 0o600); err != nil {
		t.Fatalf("write broken policy: %v", err)
	}
	settings, err = BuildSettings(PermissionOptions{ProjectDir: project, HookBinary: "orchicon", PolicyPath: broken})
	if err == nil {
		t.Fatal("a malformed policy must be reported, never silently dropped")
	}
	if settings == "" {
		t.Fatal("a malformed policy must not remove the launch restrictions")
	}
	doc := settingsDoc(t, []string{"--settings", settings})
	if !strings.Contains(SettingsHookBinary(settings), "orchicon") {
		t.Fatal("the fail-closed hook must still be installed when the policy is broken")
	}
	deny = stringSet(doc["permissions"].(map[string]any)["deny"])
	if !deny["Bash(sudo:*)"] {
		t.Fatal("the never-allow deny rules must survive a policy load failure")
	}
	// An ABSENT policy file is a legitimate state, not a failure.
	if _, err := BuildSettings(PermissionOptions{ProjectDir: project, PolicyPath: filepath.Join(t.TempDir(), "nope.yaml")}); err != nil {
		t.Errorf("an absent policy file must not be an error: %v", err)
	}
}

// argv() is the actual production launch line: it must carry the permission
// restrictions too, and still no bypass flag.
func TestSessionArgvCarriesTheRestrictions(t *testing.T) {
	project := t.TempDir()
	s := newSession(&Bridge{}, "exec-1", "tenant-1", scheduler.ExecutionManifest{ProjectDir: project}, nil)
	argv := s.argv()
	joined := strings.Join(argv, " ")
	if !contains(argv, "--permission-mode") {
		t.Fatalf("session argv has no permission mode: %v", argv)
	}
	if !contains(argv, "--settings") {
		t.Fatalf("session argv has no settings document: %v", argv)
	}
	if skipPermissions.MatchString(joined) {
		t.Fatalf("session argv carries a skip-permissions spelling: %v", argv)
	}
	if !contains(argv, "--disallowedTools") {
		t.Fatalf("session argv does not deny the built-in subagent tool: %v", argv)
	}
	// The PRODUCTION line is what actually runs, so the headline pin has to hold
	// here too — not only over PermissionArgs. Every --permission-mode value must
	// be `default` (a second, later bypass mode flag would win), and no bypass
	// spelling may appear on the line at all.
	modes := 0
	for i, a := range argv {
		if a == "--permission-mode" && i+1 < len(argv) {
			modes++
			if argv[i+1] != "default" {
				t.Fatalf("session argv carries --permission-mode %q, want %q: %v", argv[i+1], "default", argv)
			}
		}
	}
	if modes == 0 {
		t.Fatalf("session argv carries no --permission-mode: %v", argv)
	}
	for _, tok := range bypassLaunchFlags {
		if strings.Contains(strings.ToLower(joined), strings.ToLower(tok)) {
			t.Fatalf("the production session argv contains the bypass flag %q: %v", tok, argv)
		}
	}
	// The settings document the production line ships must pin the bypass mode OFF.
	settings := ""
	for i, a := range argv {
		if a == "--settings" && i+1 < len(argv) {
			settings = argv[i+1]
		}
	}
	if settings == "" {
		t.Fatalf("session argv carries no settings value: %v", argv)
	}
	perm, _ := settingsDoc(t, argv)["permissions"].(map[string]any)
	if got := perm["disableBypassPermissionsMode"]; got != "disable" {
		t.Fatalf("the production settings carry disableBypassPermissionsMode=%#v, want %q", got, "disable")
	}
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

func stringSet(v any) map[string]bool {
	out := map[string]bool{}
	switch t := v.(type) {
	case []string:
		for _, s := range t {
			out[s] = true
		}
	case []any:
		for _, e := range t {
			if s, ok := e.(string); ok {
				out[s] = true
			}
		}
	}
	return out
}
