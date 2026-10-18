//go:build windows

package desktoppreflight

import (
	"golang.org/x/sys/windows"
)

// ShowBlocking puts the preflight failure in front of the user BEFORE any window
// exists, using the one UI a Go process can raise with no webview and no console:
// a native MessageBox. This is the piece that makes "never a blank window" true on
// Windows — a stderr line would be invisible in a GUI launch.
func ShowBlocking(r Report) {
	text, err := windows.UTF16PtrFromString(r.String())
	if err != nil {
		return
	}
	caption, err := windows.UTF16PtrFromString("Orchicon cannot start")
	if err != nil {
		return
	}
	windows.MessageBox(0, text, caption, windows.MB_OK|windows.MB_ICONERROR)
}
