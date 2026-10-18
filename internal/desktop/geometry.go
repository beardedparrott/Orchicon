package desktop

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
)

// Window geometry: initial size, minimum size, and remembered size + position.
//
// THE MINIMUM IS CHOSEN, NOT DERIVED. The SPA has no responsive breakpoint to
// read one off — it is a fixed sidebar plus content columns — so below roughly
// 1024x700 the sidebar and the content cannot both hold. A window minimum is
// much cheaper than making the SPA responsive (out of scope), and it keeps the
// content area looking like the browser's.
const (
	defaultWidth  = 1440
	defaultHeight = 900
	minWidth      = 1024
	minHeight     = 700
)

// Geometry is the remembered window placement.
type Geometry struct {
	X     int  `json:"x"`
	Y     int  `json:"y"`
	W     int  `json:"w"`
	H     int  `json:"h"`
	Maxed bool `json:"maximised"`
}

// Size is a width/height pair.
type Size struct{ W, H int }

// Rect is a monitor's usable area, in the same coordinate space the window
// manager reports (Wails' runtime.Screen has Left/Top/Width/Height, negative
// for monitors left of or above the primary).
type Rect struct{ X, Y, W, H int }

// DefaultGeometry is the initial window: 1440x900, which fits the always-rendered
// sidebar plus a wide execution table without horizontal scrolling, and is
// centred on the primary monitor (X/Y left at zero here and centred by
// ClampToMonitors, which knows the monitor list).
func DefaultGeometry() Geometry {
	return Geometry{W: defaultWidth, H: defaultHeight}
}

// MinSize is the smallest window the shell will allow.
func MinSize() Size { return Size{W: minWidth, H: minHeight} }

// ConfigPath is where the geometry is remembered:
// <os.UserConfigDir()>/orchicon-desktop/window.json.
//
// KEYED BY MACHINE, NOT BY INSTANCE, on purpose: window placement is a property
// of the workstation (which screen, where), not of the plane the window points
// at, and internal/planecontrol's HostDataDir is the per-instance location for
// the plane's own state.
func ConfigPath() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "orchicon-desktop", "window.json")
}

// LoadGeometry reads remembered geometry. ok=false means "no usable record":
// the caller uses DefaultGeometry. A corrupt or empty file is not an error —
// losing the window position must never stop the app opening.
func LoadGeometry(path string) (Geometry, bool) {
	if path == "" {
		return Geometry{}, false
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return Geometry{}, false
	}
	var g Geometry
	if err := json.Unmarshal(b, &g); err != nil {
		return Geometry{}, false
	}
	if g.W <= 0 || g.H <= 0 {
		return Geometry{}, false
	}
	return g, true
}

// SaveGeometry persists geometry, creating the directory. Best-effort by
// contract: the caller logs and continues, because a read-only config dir must
// not turn a window close into a crash.
func SaveGeometry(path string, g Geometry) error {
	if path == "" {
		return errors.New("no config path")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.Marshal(g)
	if err != nil {
		return err
	}
	// Written via a temp file + rename so a crash mid-write cannot leave a
	// half-JSON file that silently discards the operator's placement next launch.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// ClampToMonitors returns geometry that is guaranteed usable on the monitors
// that currently exist — the brief's "restored only onto a visible monitor".
//
// The rules, in order:
//   - size is raised to the minimum window and, when a monitor is known,
//     capped to that monitor's extent;
//   - if the restored rect intersects NO monitor (the monitor it was placed on
//     was unplugged, or the layout changed), the POSITION is dropped and the
//     window is centred on the primary — the size the operator chose is kept;
//   - with no monitor information at all, the position is kept (a headless or
//     pre-webview call must not shuffle a perfectly good placement);
//   - a zero position on a fresh profile means "not placed yet", which is also
//     centred rather than pinned to the top-left corner.
func ClampToMonitors(g Geometry, monitors []Rect) Geometry {
	min := MinSize()
	if g.W < min.W {
		g.W = min.W
	}
	if g.H < min.H {
		g.H = min.H
	}
	if len(monitors) == 0 {
		return g
	}
	primary := monitors[0]
	if g.W > primary.W {
		g.W = primary.W
	}
	if g.H > primary.H {
		g.H = primary.H
	}
	if g.X == 0 && g.Y == 0 {
		return centred(g, primary)
	}
	for _, m := range monitors {
		if intersects(Geometry{X: g.X, Y: g.Y, W: g.W, H: g.H}, m) {
			return g
		}
	}
	return centred(g, primary)
}

// intersects reports whether the window rect overlaps the monitor rect at all.
func intersects(g Geometry, m Rect) bool {
	return g.X < m.X+m.W && m.X < g.X+g.W && g.Y < m.Y+m.H && m.Y < g.Y+g.H
}

// centred centres g on m.
func centred(g Geometry, m Rect) Geometry {
	g.X = m.X + (m.W-g.W)/2
	g.Y = m.Y + (m.H-g.H)/2
	return g
}

// geometryFileExists reports whether a remembered geometry file is present. Used
// only to distinguish "first launch" from "saved geometry" in diagnostics.
func geometryFileExists(path string) bool {
	if path == "" {
		return false
	}
	_, err := fs.Stat(os.DirFS(filepath.Dir(path)), filepath.Base(path))
	return err == nil
}
