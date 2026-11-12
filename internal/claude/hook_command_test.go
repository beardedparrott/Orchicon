package claude

// hook_command_test.go — the hook command must SURVIVE A SHELL, which is what executes it.
//
// REPORTED: "ORCHICON WORKER SUMMARY: failure — Nothing implemented or committed. Write/Edit permission was
// denied for all project files."
//
// That reads as a permission-POLICY problem and is not one. The settings document registered
//
//	"command": <hookBin> + " claude-hook"
//
// with NO QUOTING. A shell splits that at the first space, so on a Windows install
// (`C:\Program Files\Orchicon\orchicon.exe`) the command became `C:\Program` plus arguments — the hook never
// ran, and claude treats a FAILING PreToolUse hook as a BLOCK. Every gated tool was refused, which the model
// reports as denied permission.
//
// PreflightHookExecutable cannot catch it: it stats the FILE (absolute? readable? not a directory?) and a
// path with spaces satisfies every check. The breakage only exists at shell-split time, which is why these
// assertions are about the COMMAND STRING rather than about the filesystem.

import (
	"encoding/json"
	"strings"
	"testing"
)

// A path WITH SPACES must come out quoted and intact.
func TestHookCommandQuotesAPathWithSpaces(t *testing.T) {
	const winPath = `C:\Program Files\Orchicon\orchicon.exe`

	settings, err := BuildSettings(PermissionOptions{ProjectDir: t.TempDir(), HookBinary: winPath})
	if err != nil {
		t.Fatalf("BuildSettings: %v", err)
	}
	cmd := preToolUseCommand(t, settings)

	// THE WHOLE PATH, UNSPLIT. An unquoted command starts with `C:\Program` and this fails.
	if !strings.HasPrefix(cmd, `"`+winPath+`"`) {
		t.Fatalf("hook command = %q, want it to start with the QUOTED binary path %q.\n"+
			"  An unquoted path is split at its first space by the shell that runs the command, the hook "+
			"never runs, and claude blocks every gated tool — reported as a permission denial.",
			cmd, `"`+winPath+`"`)
	}
	if !strings.HasSuffix(cmd, " claude-hook") {
		t.Errorf("hook command = %q, want it to still invoke `claude-hook`", cmd)
	}
}

// The POSIX shape must be quoted too — not because it needs it, but because a conditional would leave the
// next unusual path uncovered, and quoting a path without spaces changes nothing.
func TestHookCommandQuotesAPlainPath(t *testing.T) {
	settings, err := BuildSettings(PermissionOptions{ProjectDir: t.TempDir(), HookBinary: "/usr/local/bin/orchicon"})
	if err != nil {
		t.Fatalf("BuildSettings: %v", err)
	}
	if got, want := preToolUseCommand(t, settings), `"/usr/local/bin/orchicon" claude-hook`; got != want {
		t.Fatalf("hook command = %q, want %q", got, want)
	}
}

// The Ask profile registers a SECOND hook (PermissionRequest) and it must be quoted identically — two
// commands, one renderer, or one of them drifts back to the defect.
func TestEveryHookCommandIsQuoted(t *testing.T) {
	const winPath = `C:\Program Files\Orchicon\orchicon.exe`
	settings, err := BuildSettings(PermissionOptions{
		ProjectDir: t.TempDir(),
		HookBinary: winPath,
		Profile:    ProfileAskEnvValue,
	})
	if err != nil {
		t.Fatalf("BuildSettings: %v", err)
	}

	var doc struct {
		Hooks map[string][]struct {
			Hooks []struct {
				Command string `json:"command"`
			} `json:"hooks"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal([]byte(settings), &doc); err != nil {
		t.Fatalf("decode settings: %v", err)
	}

	commands := 0
	for hookKind, matchers := range doc.Hooks {
		for _, m := range matchers {
			for _, h := range m.Hooks {
				if !strings.Contains(h.Command, "claude-hook") {
					continue
				}
				commands++
				// EVERY command, whatever hook kind it belongs to. A future hook added with the raw
				// `hookBin + " claude-hook"` form fails HERE rather than on an operator's Windows box.
				if !strings.HasPrefix(h.Command, `"`+winPath+`"`) {
					t.Errorf("%s hook command = %q, want the QUOTED binary path — an unquoted path is split "+
						"at its first space, the hook never runs, and claude blocks the tool",
						hookKind, h.Command)
				}
			}
		}
	}
	if commands < 2 {
		t.Fatalf("found %d hook commands, want at least the PreToolUse and PermissionRequest hooks", commands)
	}
}

// SettingsHookBinary must return the PATH, not the quoted command — every caller compares it to a path.
func TestSettingsHookBinaryUnquotesThePath(t *testing.T) {
	const winPath = `C:\Program Files\Orchicon\orchicon.exe`
	settings, err := BuildSettings(PermissionOptions{ProjectDir: t.TempDir(), HookBinary: winPath})
	if err != nil {
		t.Fatalf("BuildSettings: %v", err)
	}
	if got := SettingsHookBinary(settings); got != winPath {
		t.Fatalf("SettingsHookBinary = %q, want the bare path %q — a caller comparing this to a path would "+
			"see a leading quote and disagree with itself", got, winPath)
	}
}

// preToolUseCommand returns the PreToolUse hook's command from a settings document.
func preToolUseCommand(t *testing.T, settings string) string {
	t.Helper()
	var doc struct {
		Hooks struct {
			PreToolUse []struct {
				Hooks []struct {
					Command string `json:"command"`
				} `json:"hooks"`
			} `json:"PreToolUse"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal([]byte(settings), &doc); err != nil {
		t.Fatalf("decode settings: %v", err)
	}
	for _, m := range doc.Hooks.PreToolUse {
		for _, h := range m.Hooks {
			if h.Command != "" {
				return h.Command
			}
		}
	}
	t.Fatalf("no PreToolUse hook command in settings: %s", settings)
	return ""
}
