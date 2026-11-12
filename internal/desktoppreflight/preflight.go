// Package desktoppreflight answers ONE question before the desktop app creates a window: can this
// host render a webview?
package desktoppreflight

import (
	"fmt"
	"runtime"
	"strings"
)

// WebView2EvergreenURL is Microsoft's stable "Evergreen bootstrapper" link.
const WebView2EvergreenURL = "https://go.microsoft.com/fwlink/p/?LinkId=2124703"

// Report is the outcome of a preflight check.
type Report struct {
	OK       bool
	Platform string
	Webview  string
	Detail   string
	Remedy   string
}

// Probe is every OS-specific capability the decision needs, injected.
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

// Evaluate is the whole decision, as a pure function of the platform name and the injected probe
// results.
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

// evaluateWindows reads the four registry locations the WebView2 runtime registers itself in, then
// falls back to the install directory.
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

// webkitSonames are the two sonames a working WebKitGTK install leaves behind: the 4.0 series
// (Ubuntu <= 23.10, Debian 12) and the 4.1 series (Ubuntu 24.04+).
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
		// NO PROBER IS NOT PROOF OF ABSENCE.
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
