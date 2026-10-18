package desktoppreflight

import (
	"errors"
	"strings"
	"testing"
)

// fakeProbe builds a Probe whose every capability is a table-controlled answer, so
// the decision can be exercised on a CI runner that has no webview, no ldconfig
// output worth trusting and no Windows registry at all.
type fakeProbe struct {
	have     []string // binaries Lookup resolves
	run      map[string]string
	runErr   map[string]error
	env      map[string]string
	exists   []string // paths Stat reports present
	registry map[string]string
}

func (f fakeProbe) probe() Probe {
	return Probe{
		Lookup: func(name string) (string, error) {
			for _, h := range f.have {
				if h == name {
					return "/usr/bin/" + name, nil
				}
			}
			return "", errors.New("not found")
		},
		Run: func(name string, args ...string) (string, error) {
			key := strings.Join(append([]string{name}, args...), " ")
			if err, ok := f.runErr[key]; ok {
				return "", err
			}
			if out, ok := f.run[key]; ok {
				return out, nil
			}
			return "", errors.New("no output")
		},
		Env: func(k string) string { return f.env[k] },
		Stat: func(path string) (bool, error) {
			for _, p := range f.exists {
				if p == path {
					return true, nil
				}
			}
			return false, errors.New("no such file")
		},
		Registry: func(root, path, value string) (string, bool) {
			v, ok := f.registry[root+`\`+path+`#`+value]
			return v, ok
		},
	}
}

const pvKey = `\SOFTWARE\WOW6432Node\Microsoft\EdgeUpdate\Clients\{F3017226-FE2A-4295-8BDF-00C3A9A7E4C5}`

// TestEvaluateLinuxWebKitGTK covers every way the Linux branch can be decided: the
// two sonames (a 4.0 distro and a 4.1 distro), the pkg-config fallback, a loader
// that has neither, and a host with no prober at all.
func TestEvaluateLinuxWebKitGTK(t *testing.T) {
	cases := []struct {
		name       string
		p          fakeProbe
		wantOK     bool
		wantDetail string // substring
	}{
		{
			name: "webkitgtk 4.0 present in the loader cache",
			p: fakeProbe{
				have: []string{"ldconfig"},
				run:  map[string]string{"ldconfig -p": "libwebkit2gtk-4.0.so.37 (libc6,x86-64) => /usr/lib/libwebkit2gtk-4.0.so.37\n"},
			},
			wantOK:     true,
			wantDetail: "libwebkit2gtk-4.0.so.37",
		},
		{
			name: "webkitgtk 4.1 present (Ubuntu 24.04 and later)",
			p: fakeProbe{
				have: []string{"ldconfig"},
				run:  map[string]string{"ldconfig -p": "libwebkit2gtk-4.1.so.37 (libc6,x86-64)\n"},
			},
			wantOK:     true,
			wantDetail: "libwebkit2gtk-4.1.so.37",
		},
		{
			name: "loader cache has neither, but pkg-config knows the module",
			p: fakeProbe{
				have: []string{"ldconfig", "pkg-config"},
				run:  map[string]string{"ldconfig -p": "libgtk-3.so.0 (libc6,x86-64)\n"},
			},
			// pkg-config --exists webkit2gtk-4.0 is absent from run, so it "fails":
			// the Run stub returns an error for an unknown command line.
			wantOK:     false,
			wantDetail: "neither libwebkit2gtk-4.0.so.37 nor libwebkit2gtk-4.1.so.37",
		},
		{
			name: "no webview anywhere",
			p: fakeProbe{
				have: []string{"ldconfig"},
				run:  map[string]string{"ldconfig -p": "libgtk-3.so.0 (libc6,x86-64)\n"},
			},
			wantOK:     false,
			wantDetail: "cannot draw a window",
		},
		{
			name:       "ldconfig missing entirely and no pkg-config — unprobeable, not absent",
			p:          fakeProbe{},
			wantOK:     true,
			wantDetail: "no loader probe available",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Evaluate("linux", tc.p.probe())
			if got.OK != tc.wantOK {
				t.Fatalf("OK = %v, want %v (report: %s)", got.OK, tc.wantOK, got.String())
			}
			if !strings.Contains(got.Detail, tc.wantDetail) {
				t.Errorf("detail %q does not contain %q", got.Detail, tc.wantDetail)
			}
			if got.Webview != "WebKitGTK" {
				t.Errorf("Webview = %q, want WebKitGTK — the docs and the remedy name it", got.Webview)
			}
		})
	}
}

// TestEvaluateLinuxPkgConfigFallback is separated because it needs a SUCCEEDING
// pkg-config call, which the table's Run stub cannot express alongside a failing one.
func TestEvaluateLinuxPkgConfigFallback(t *testing.T) {
	p := fakeProbe{
		have: []string{"pkg-config"},
		run:  map[string]string{"pkg-config --exists webkit2gtk-4.1": ""},
	}.probe()
	got := Evaluate("linux", p)
	if !got.OK {
		t.Fatalf("OK = false, want true when pkg-config reports webkit2gtk-4.1: %s", got.String())
	}
	if !strings.Contains(got.Detail, "webkit2gtk-4.1") {
		t.Errorf("detail %q should name the module pkg-config matched", got.Detail)
	}
}

// TestEvaluateWindowsWebView2 covers the four registry locations, the filesystem
// fallback, and the not-installed case — the last of which is the Windows 10
// machine this whole work item exists for.
func TestEvaluateWindowsWebView2(t *testing.T) {
	cases := []struct {
		name       string
		p          fakeProbe
		wantOK     bool
		wantDetail string
	}{
		{
			name:       "runtime registered under HKLM WOW6432Node",
			p:          fakeProbe{registry: map[string]string{"HKLM" + pvKey + "#pv": "120.0.2210.91"}},
			wantOK:     true,
			wantDetail: "120.0.2210.91",
		},
		{
			name:       "runtime registered per-user under HKCU",
			p:          fakeProbe{registry: map[string]string{"HKCU" + pvKey + "#pv": "121.0.2277.83"}},
			wantOK:     true,
			wantDetail: "121.0.2277.83",
		},
		{
			name: "registry unreadable but the install directory exists",
			p: fakeProbe{
				env:    map[string]string{"ProgramFiles(x86)": `C:\Program Files (x86)`},
				exists: []string{`C:\Program Files (x86)\Microsoft\EdgeWebView\Application`},
			},
			wantOK:     true,
			wantDetail: "runtime directory found",
		},
		{
			name:       "nothing installed — Windows 10 first run",
			p:          fakeProbe{env: map[string]string{"ProgramFiles": `C:\Program Files`}},
			wantOK:     false,
			wantDetail: "was not found",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Evaluate("windows", tc.p.probe())
			if got.OK != tc.wantOK {
				t.Fatalf("OK = %v, want %v (report: %s)", got.OK, tc.wantOK, got.String())
			}
			if !strings.Contains(got.Detail, tc.wantDetail) {
				t.Errorf("detail %q does not contain %q", got.Detail, tc.wantDetail)
			}
		})
	}
}

// TestDarwinIsAlwaysOK pins the decision that macOS needs no webview probe: WKWebView
// is part of the OS, and the macOS user-facing risk is signing, not the webview. If
// this ever needs to fail, the remedy text has to change with it.
func TestDarwinIsAlwaysOK(t *testing.T) {
	got := Evaluate("darwin", Probe{})
	if !got.OK {
		t.Fatalf("darwin preflight must always pass, got: %s", got.String())
	}
	if !strings.Contains(got.Webview, "WKWebView") {
		t.Errorf("Webview = %q, want it to name WKWebView", got.Webview)
	}
	if got.RemedyURL() != "" {
		t.Errorf("macOS has no download remedy, got %q", got.RemedyURL())
	}
}

// TestUnknownPlatformFailsClosed: a build for a platform with no check must refuse
// rather than open a window it cannot fill.
func TestUnknownPlatformFailsClosed(t *testing.T) {
	got := Evaluate("plan9", Probe{})
	if got.OK {
		t.Fatal("an unknown platform must fail closed — proceeding is how a user gets a blank window")
	}
	if !strings.Contains(got.String(), "plan9") {
		t.Errorf("the message must name the platform: %s", got.String())
	}
}

// TestReportStringNamesTheFix is AC3's real assertion: the failure message must carry
// something the user can DO. An opaque "cannot start" is the bug this package fixes.
func TestReportStringNamesTheFix(t *testing.T) {
	const aptCmd = "sudo apt install libwebkit2gtk-4.0-37"
	linux := Evaluate("linux", fakeProbe{
		have: []string{"ldconfig"},
		run:  map[string]string{"ldconfig -p": ""},
	}.probe())
	if linux.OK {
		t.Fatal("expected a failing Linux report")
	}
	if !strings.Contains(linux.String(), aptCmd) {
		t.Errorf("the Linux failure must name the exact command (%q): %s", aptCmd, linux.String())
	}
	if !strings.Contains(linux.String(), "WebKitGTK") {
		t.Errorf("the Linux failure must name WebKitGTK: %s", linux.String())
	}

	windows := Evaluate("windows", Probe{})
	if windows.OK {
		t.Fatal("expected a failing Windows report")
	}
	if !strings.Contains(windows.String(), WebView2EvergreenURL) {
		t.Errorf("the Windows failure must carry the Evergreen bootstrapper URL: %s", windows.String())
	}
	if windows.RemedyURL() != WebView2EvergreenURL {
		t.Errorf("RemedyURL = %q, want %q", windows.RemedyURL(), WebView2EvergreenURL)
	}
}

// TestOKReportIsOneLine keeps the success path out of the way: it is written to
// stderr before every launch, so it must not be a paragraph.
func TestOKReportIsOneLine(t *testing.T) {
	ok := Evaluate("darwin", Probe{})
	if strings.Contains(strings.TrimSpace(ok.String()), "\n") {
		t.Errorf("the success report must be a single line, got %q", ok.String())
	}
}
