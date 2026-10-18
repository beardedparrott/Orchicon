package desktop

import "fmt"

// Title is the window title: the app name AND the instance it points at
// ("Orchicon — dev (127.0.0.1:8080)").
//
// WHY THE INSTANCE IS IN THE TITLE: the desktop app is a thin client over a
// separately running plane, and the same window can be pointed at dev, prod or
// test. Without the instance in the title, a destructive-looking action against
// prod is indistinguishable from the same action against dev — which is the one
// mistake this app makes possible that the browser does not.
//
// Unknown port: the instance reads "unknown" and the raw host:port is shown, so
// "not one of our three instances" is visible rather than guessed at.
func Title(info Info, hostPort string) string {
	instance := info.Instance
	if instance == "" {
		instance = UnknownInstance
	}
	if hostPort == "" {
		hostPort = info.PlaneURL
	}
	return fmt.Sprintf("Orchicon — %s (%s)", instance, hostPort)
}

// WindowOptions is the shell's window specification, as one value so the entry
// point cannot pick a different title or size than the one documented in
// docs/desktop-app.md.
type WindowOptions struct {
	Title string
	W, H  int
	MinW  int
	MinH  int
}

// WindowOptionsFor builds the window options from resolved geometry and Info.
// Geometry wins over the defaults (remembered size), and the minimum is applied
// by ClampToMonitors already — MinW/MinH restate it so the window manager
// enforces it on a manual resize too.
func WindowOptionsFor(info Info, hostPort string, g Geometry) WindowOptions {
	min := MinSize()
	return WindowOptions{
		Title: Title(info, hostPort),
		W:     g.W,
		H:     g.H,
		MinW:  min.W,
		MinH:  min.H,
	}
}
