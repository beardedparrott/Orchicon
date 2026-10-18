// Package desktoppreflight answers ONE question before the desktop app creates a
// window: can this host render a webview?
//
// It exists as a plain Go package (no Wails import) for two reasons:
//
//   - it must run BEFORE wails.Run, which is the only moment a check is useful —
//     after that the framework has already tried to open a window and failed
//     opaquely, which is exactly the symptom this package exists to prevent;
//   - it must be unit-testable on a CI runner that has no webview installed, so
//     every OS capability is injected through Probe and the decision is a pure
//     function of (goos, probe results).
//
// The three platforms have three different shapes of risk, and the package
// names each of them rather than pretending they are one problem:
//
//   - macOS: WKWebView is part of the OS, so the check always passes. The real
//     macOS risk is signing/notarisation, which is a release-pipeline concern
//     (deploy/desktop/macos/sign-and-notarize.sh), not a runtime probe.
//   - Windows: WebView2 ships with Windows 11 but NOT every Windows 10 install,
//     so the probe reads the Evergreen registry keys (and the filesystem as a
//     fallback) and the remedy is the Evergreen bootstrapper URL.
//   - Linux: WebKitGTK is a DYNAMIC-LINK dependency, so the dynamic loader fails
//     before main() ever runs — no in-process check can help there. The real
//     Linux detection therefore lives in the /usr/bin/orchicon-desktop launcher
//     wrapper and the AppImage's AppRun (see deploy/desktop/). What this package
//     adds on Linux is the check for a developer running a locally built binary.
package desktoppreflight

import (
	"fmt"
	"runtime"
	"strings"
)

// WebView2EvergreenURL is Microsoft's stable "Evergreen bootstrapper" link. It is
// the remedy both the Windows MessageBox and the docs point at, because it is the
// only URL that survives Microsoft renaming the download page.
const WebView2EvergreenURL = "https://go.microsoft.com/fwlink/p/?LinkId=2124703"

// Report is the outcome of a preflight check. OK is the only field the caller
// must branch on; the rest exists so the failure can be explained to a user in
// terms they can act on (a message with no runnable command or URL is the
// opaque failure this package exists to remove).
type Report struct {
	OK       bool
	Platform string
	Webview  string
	Detail   string
	Remedy   string
}

// Probe is every OS-specific capability the decision needs, injected. A nil field
// means "this capability is unavailable on this host", which the decision
// functions treat as "could not probe" rather than as "absent".
type Probe struct {
	Lookup   func(name string) (string, error)
	Run      func(name string, args ...string) (string, error)
	Env      func(string) string
	Stat     func(path string) (bool, error)
	Registry func(root, path, value string) (string, bool)
}

// Check runs the preflight for the platform this binary was built for. It is the
// only entry point the desktop shell calls.
func Check() Report { return Evaluate(runtime.GOOS, osProbe()) }

// Evaluate is the whole decision, as a pure function of the platform name and the
// injected probe results. Every branch below is covered by preflight_test.go,
// which is why the tests run on a Linux CI runner with no webview present.
//
// An UNKNOWN platform FAILS CLOSED: a build for some other GOOS cannot be known to
// render a webview, and silently proceeding is how a user gets a blank window.
func Evaluate(goos string, p Probe) Report {
	switch goos {
	case "darwin":
		return Report{
			OK:       true,
			Platform: "macOS",
			Webview:  "WKWebView (in-OS)",
			Detail:   "WKWebView ships with macOS, so nothing needs installing",
		}
	case "windows":
		return evaluateWindows(p)
	case "linux":
		return evaluateLinux(p)
	default:
		return Report{
			Platform: goos,
			Webview:  "an unknown webview",
			Detail: fmt.Sprintf("Orchicon has no webview check for %q, so it cannot promise the app will "+
				"draw a window on this platform", goos),
			Remedy: "use a release artifact for macOS, Windows or Linux",
		}
	}
}

// evaluateWindows reads the four registry locations the WebView2 runtime registers
// itself in, then falls back to the install directory. Both are needed: the
// registry is authoritative, but a machine where the runtime was updated by hand
// can have the directory without a readable key.
func evaluateWindows(p Probe) Report {
	r := Report{
		Platform: "Windows",
		Webview:  "Microsoft Edge WebView2 Runtime",
		Remedy: "install it (the Evergreen bootstrapper, ~2 MB, no admin prompt on most machines): " +
			WebView2EvergreenURL,
	}
	for _, root := range []string{"HKLM", "HKCU"} {
		for _, path := range []string{webView2KeyWOW64, webView2KeyNative} {
			if p.Registry == nil {
				break
			}
			if v, ok := p.Registry(root, path, "pv"); ok && strings.TrimSpace(v) != "" {
				r.OK = true
				r.Detail = fmt.Sprintf("WebView2 runtime %s is installed (%s\\%s)", v, root, path)
				return r
			}
		}
	}
	if p.Stat != nil {
		for _, envName := range []string{"ProgramFiles(x86)", "ProgramFiles"} {
			if base := envOr(p.Env, envName); base != "" {
				if ok, _ := p.Stat(base + `\Microsoft\EdgeWebView\Application`); ok {
					r.OK = true
					r.Detail = fmt.Sprintf("WebView2 runtime directory found under %s", base)
					return r
				}
			}
		}
	}
	r.Detail = "the WebView2 runtime was not found, so the app cannot draw a window " +
		"(this is normal on a Windows 10 machine that has never run a WebView2 app)"
	return r
}

const (
	webView2KeyWOW64  = `SOFTWARE\WOW6432Node\Microsoft\EdgeUpdate\Clients\{F3017226-FE2A-4295-8BDF-00C3A9A7E4C5}`
	webView2KeyNative = `SOFTWARE\Microsoft\EdgeUpdate\Clients\{F3017226-FE2A-4295-8BDF-00C3A9A7E4C5}`
)

// webkitSonames are the two sonames a working WebKitGTK install leaves behind: the
// 4.0 series (Ubuntu <= 23.10, Debian 12) and the 4.1 series (Ubuntu 24.04+). Both
// mean "a webview is present"; which one the binary links against is a build-time
// decision, not something this probe should second-guess.
var webkitSonames = []string{"libwebkit2gtk-4.0.so.37", "libwebkit2gtk-4.1.so.37"}

func evaluateLinux(p Probe) Report {
	r := Report{
		Platform: "Linux",
		Webview:  "WebKitGTK",
		Remedy: "install it: sudo apt install libwebkit2gtk-4.0-37   " +
			"(Fedora: sudo dnf install webkit2gtk4.0; Arch: sudo pacman -S webkit2gtk)",
	}
	probed := false
	if p.Lookup != nil && p.Run != nil {
		if _, err := p.Lookup("ldconfig"); err == nil {
			probed = true
			if out, err := p.Run("ldconfig", "-p"); err == nil {
				for _, so := range webkitSonames {
					if strings.Contains(out, so) {
						r.OK = true
						r.Detail = "found " + so + " in the loader cache"
						return r
					}
				}
			}
		}
		if _, err := p.Lookup("pkg-config"); err == nil {
			probed = true
			for _, mod := range []string{"webkit2gtk-4.0", "webkit2gtk-4.1"} {
				if _, err := p.Run("pkg-config", "--exists", mod); err == nil {
					r.OK = true
					r.Detail = "pkg-config reports " + mod + " present"
					return r
				}
			}
		}
	}
	if !probed {
		// NO PROBER IS NOT PROOF OF ABSENCE. On Linux the shipped binary cannot even
		// be loaded without WebKitGTK (the launcher wrapper and the AppImage's AppRun
		// check it first), so a host with no ldconfig and no pkg-config is a build
		// environment, not a user about to hit a blank window. Blocking here would
		// refuse to start on a working machine, which is worse than the risk.
		r.OK = true
		r.Detail = "no loader probe available (ldconfig and pkg-config are both missing); " +
			"the orchicon-desktop launcher enforces WebKitGTK before the app starts"
		return r
	}
	r.Detail = "neither libwebkit2gtk-4.0.so.37 nor libwebkit2gtk-4.1.so.37 is on this system, " +
		"so the app cannot draw a window"
	return r
}

func envOr(env func(string) string, name string) string {
	if env == nil {
		return ""
	}
	return strings.TrimSpace(env(name))
}

// RemedyURL is the download the remedy points at, when there is one. Only Windows
// has one: macOS needs nothing and on Linux the remedy is a package command.
func (r Report) RemedyURL() string {
	if r.Platform == "Windows" {
		return WebView2EvergreenURL
	}
	return ""
}

// String renders the one message a user sees. It is deliberately three lines: what
// is missing, why that matters, and the exact command or URL that fixes it.
func (r Report) String() string {
	if r.OK {
		return fmt.Sprintf("Orchicon: %s available on %s (%s).", r.Webview, r.Platform, r.Detail)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Orchicon cannot start the desktop app: %s needs %s.\n", r.Platform, r.Webview)
	if r.Detail != "" {
		fmt.Fprintf(&b, "  %s\n", r.Detail)
	}
	if r.Remedy != "" {
		fmt.Fprintf(&b, "  fix: %s\n", r.Remedy)
	}
	if u := r.RemedyURL(); u != "" && !strings.Contains(r.Remedy, u) {
		fmt.Fprintf(&b, "  download: %s\n", u)
	}
	return b.String()
}
