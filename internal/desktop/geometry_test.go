package desktop

import (
	"path/filepath"
	"testing"
)

// AC7: the window has an initial size and a minimum that keeps the SPA's
// layouts intact.
func TestDefaultAndMinSize(t *testing.T) {
	g := DefaultGeometry()
	if g.W != 1440 || g.H != 900 {
		t.Errorf("DefaultGeometry = %dx%d, want 1440x900", g.W, g.H)
	}
	min := MinSize()
	if min.W < 1024 || min.H < 700 {
		t.Errorf("MinSize = %dx%d — the SPA's sidebar plus content columns cannot both hold in a smaller window", min.W, min.H)
	}
	if min.W > g.W || min.H > g.H {
		t.Errorf("MinSize %v exceeds the initial size %dx%d", min, g.W, g.H)
	}
}

// AC7: remembered geometry round-trips.
func TestGeometryRoundTrips(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "window.json")
	want := Geometry{X: 120, Y: 80, W: 1600, H: 1000}
	if err := SaveGeometry(path, want); err != nil {
		t.Fatalf("SaveGeometry: %v", err)
	}
	got, ok := LoadGeometry(path)
	if !ok || got != want {
		t.Fatalf("LoadGeometry = %+v, %v; want %+v, true", got, ok, want)
	}
	// A missing or corrupt record is never an error: losing the window position
	// must not stop the app opening.
	if _, ok := LoadGeometry(filepath.Join(t.TempDir(), "absent.json")); ok {
		t.Error("LoadGeometry reported ok for a file that does not exist")
	}
	if _, ok := LoadGeometry(""); ok {
		t.Error("LoadGeometry reported ok for an empty path")
	}
}

// AC7's exact requirement: restored ONLY onto a visible monitor.
func TestClampToMonitors(t *testing.T) {
	primary := Rect{X: 0, Y: 0, W: 1920, H: 1080}
	second := Rect{X: 1920, Y: 0, W: 1920, H: 1080}

	// Visible on the second monitor: untouched.
	visible := Geometry{X: 2000, Y: 100, W: 1440, H: 900}
	if got := ClampToMonitors(visible, []Rect{primary, second}); got != visible {
		t.Errorf("visible geometry was moved: %+v -> %+v", visible, got)
	}

	// Remembered on a monitor that is gone: the position is dropped, the SIZE is
	// kept, and the window is centred on the primary.
	stranded := Geometry{X: 5000, Y: 3000, W: 1600, H: 1000}
	got := ClampToMonitors(stranded, []Rect{primary})
	if got.X != 160 || got.Y != 40 {
		t.Errorf("stranded geometry = %+v, want it centred on the primary (160,40)", got)
	}
	if got.W != 1600 || got.H != 1000 {
		t.Errorf("stranded geometry changed SIZE (%dx%d) — only the position should be dropped", got.W, got.H)
	}

	// A window below the minimum is raised to it.
	small := ClampToMonitors(Geometry{X: 10, Y: 10, W: 200, H: 200}, []Rect{primary})
	if small.W != MinSize().W || small.H != MinSize().H {
		t.Errorf("small geometry = %dx%d, want the minimum %v", small.W, small.H, MinSize())
	}

	// No monitor information at all: keep the placement (a headless caller must
	// not shuffle a perfectly good position).
	keep := Geometry{X: 300, Y: 200, W: 1200, H: 800}
	if got := ClampToMonitors(keep, nil); got != keep {
		t.Errorf("geometry moved with no monitors known: %+v -> %+v", keep, got)
	}

	// A fresh profile (0,0) is centred rather than pinned to the top-left.
	if got := ClampToMonitors(DefaultGeometry(), []Rect{primary}); got.X == 0 && got.Y == 0 {
		t.Error("fresh geometry stayed at 0,0 instead of being centred on the primary monitor")
	}

	// THE CAP FOLLOWS THE MONITOR THE WINDOW IS ON, not the primary: a window
	// remembered on a larger second screen keeps its size, or a maximised window
	// would shrink on every launch on a mixed-size desk.
	big := Rect{X: 1920, Y: 0, W: 2560, H: 1440}
	onBig := Geometry{X: 2000, Y: 100, W: 2400, H: 1300}
	if got := ClampToMonitors(onBig, []Rect{primary, big}); got != onBig {
		t.Errorf("geometry on the larger second monitor = %+v, want it kept as %+v — the size cap must follow "+
			"the monitor the window is on, not the primary", got, onBig)
	}

	// Straddling two screens: capped to the LARGEST it covers, not the smaller.
	straddling := Geometry{X: 1900, Y: 100, W: 2500, H: 1400}
	if got := ClampToMonitors(straddling, []Rect{primary, big}); got != straddling {
		t.Errorf("straddling geometry = %+v, want %+v (capped to the largest monitor it covers)", got, straddling)
	}
}
