package desktop

import "fmt"

// Title is the window title: the app name AND the instance it points at ("Orchicon — dev
// (127.0.0.1:8080)").
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

// WindowOptions is the shell's window specification, as one value so the entry point cannot pick a
// different title or size than the one documented in docs/desktop-app.md.
type WindowOptions struct {
	Title string
	W, H  int
	MinW  int
	MinH  int
}

// WindowOptionsFor builds the window options from resolved geometry and Info.
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
