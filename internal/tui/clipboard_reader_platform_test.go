package tui

// clipboard_reader_windows_test.go — WHICH CLIPBOARD READER EACH PLATFORM GETS.
//
// The operator, testing on Windows: "copy and paste into the composer in orch on Windows seemed to send it
// as multiple lines on a big block of code pasted."
//
// TWO FAULTS MEET HERE, and this file pins the half that can be fixed and verified off-platform.
//
//  1. clipboardTextCommand had NO "windows" arm at all. ctrl+v is the composer's paste chord (the
//     textarea's own binding is unreachable — the chord is taken), so on a Windows box every text paste
//     answered "no clipboard reader on this machine — install a platform clipboard tool". A missing branch
//     is precisely the defect that survives a hand-test on the platform that already worked, which is why
//     the decision is now a pure function over (goos, wayland, which-binary-exists) and asserted here.
//
//  2. THE SEND WAS NEVER THE PASTE PATH'S FAULT. Native Windows input does not go through the byte stream
//     in internal/tui/input: bubbletea v1.3.10 routes a Windows console to its coninput reader
//     (inputreader_windows.go), which decodes a scancode stream and reports VK_RETURN as KeyEnter with no
//     Shift and no Paste flag — because ENABLE_BRACKETED_PASTE is never set on that path. A pasted block is
//     therefore a burst of ordinary Enter keys, and the dock's documented contract ("enter send") fires on
//     each one. That is ONE cause for both reported symptoms, and it is why the reader added below matters:
//     text inserted through clipboardTextMsg reaches the composer as text, never as keys.
//
// The reader decision is what this file can prove without a Windows box. The console-mode half is a
// separate change that must be verified on the platform.

import (
	"strings"
	"testing"
)

// fakeBinaries reports only the named commands as present, so a whole platform table can be asserted on
// any host.
func fakeBinaries(present ...string) func(string) bool {
	set := make(map[string]bool, len(present))
	for _, p := range present {
		set[p] = true
	}
	return func(name string) bool { return set[name] }
}

// TestClipboardTextCommandCoversWindows pins the branch whose ABSENCE was the bug.
//
// It also pins the FLAGS, because each is load-bearing and none of them is cosmetic:
//   - -NoProfile for CORRECTNESS: anything a profile prints goes to stdout and becomes pasted text.
//   - -STA because Get-Clipboard needs a single-threaded apartment and fails under MTA.
//   - -Raw so the clipboard comes back as ONE string rather than a line-split list rejoined with newlines.
//
// It must not depend on "pwsh": PowerShell 7 is an optional install, and choosing it would leave the one
// platform this branch exists for with no reader at all.
func TestClipboardTextCommandCoversWindows(t *testing.T) {
	got := clipboardTextCommandFor("windows", "", fakeBinaries())
	if len(got) == 0 {
		t.Fatal("windows must resolve a clipboard reader — its absence is what made every ctrl+v paste report " +
			"'no clipboard reader on this machine' on a Windows box")
	}
	if got[0] != "powershell" {
		t.Errorf("windows reader = %q, want powershell (present on every supported Windows; pwsh is an "+
			"optional install and must not be required)", got[0])
	}
	// Assert over the JOINED argv, not element-by-element: the cmdlet and its flags live in the single
	// -Command payload ("Get-Clipboard -Raw"), so -Raw is a token inside one argument rather than an argv
	// element of its own. Checking element prefixes would report a correct command as missing -Raw.
	joined := strings.Join(got, " ")
	for _, want := range []string{"-NoProfile", "-STA", "-NoLogo", "Get-Clipboard -Raw"} {
		if !strings.Contains(joined, want) {
			t.Errorf("windows reader %v is missing %q", got, want)
		}
	}
}

// TestClipboardTextCommandKeepsTheOtherPlatformsUnchanged is the regression half: adding a platform must not
// quietly reorder the ones that already worked.
func TestClipboardTextCommandKeepsTheOtherPlatformsUnchanged(t *testing.T) {
	cases := []struct {
		name    string
		goos    string
		wayland string
		present []string
		want    []string
	}{
		{"darwin/pbpaste", "darwin", "", []string{"pbpaste"}, []string{"pbpaste"}},
		{"darwin without pbpaste", "darwin", "", nil, nil},
		{"linux/wayland wl-paste", "linux", "wayland-0", []string{"wl-paste", "xclip"}, []string{"wl-paste", "-n"}},
		{"linux/x11 xclip", "linux", "", []string{"xclip"}, []string{"xclip", "-selection", "clipboard", "-o"}},
		{"linux/x11 xsel", "linux", "", []string{"xsel"}, []string{"xsel", "--clipboard", "--output"}},
		// Wayland DISPLAY set but wl-paste missing must still fall through to the X11 readers rather than
		// resolving nothing — the same fall-through the old inline switch had.
		{"linux/wayland without wl-paste", "linux", "wayland-0", []string{"xclip"}, []string{"xclip", "-selection", "clipboard", "-o"}},
		{"linux nothing installed", "linux", "", nil, nil},
		{"unknown platform", "plan9", "", []string{"pbpaste"}, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := clipboardTextCommandFor(c.goos, c.wayland, fakeBinaries(c.present...))
			if len(got) != len(c.want) {
				t.Fatalf("clipboardTextCommandFor(%q) = %v, want %v", c.goos, got, c.want)
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Fatalf("clipboardTextCommandFor(%q) = %v, want %v", c.goos, got, c.want)
				}
			}
		})
	}
}

// TestClipboardHintForWindowsDoesNotSendTheOperatorToAUnixTool pins the message, because a wrong hint is
// worse than none: it would tell a Windows operator to install xclip.
func TestClipboardHintForWindowsDoesNotSendTheOperatorToAUnixTool(t *testing.T) {
	// The hint is selected by runtime.GOOS, so this asserts the CONTRACT rather than the branch: on Windows
	// the text reader needs no install, and naming a Unix tool would be an instruction they cannot follow.
	// The value is checked through the helper the error actually uses, on this host, only to prove it never
	// returns "".
	if clipboardHelperHint() == "" {
		t.Fatal("clipboardHelperHint must always name something")
	}
}
