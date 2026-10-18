//go:build desktop

// orchicon-desktop is the thin Wails entry point: it owns NO logic. Everything
// testable lives in internal/desktop (the loopback origin, the proxy, the
// geometry, the title), because a webview cannot be driven from CI and the
// transport is the risky part of this app.
//
// THE ONE WAILS-SPECIFIC HOP is the asset handler: the webview's document is
// loaded initially from Wails' own scheme, and that handler immediately 302s to
// the shell's real http://127.0.0.1:<ephemeral> origin. That 302 is what buys
// cookie-bearing auth (the HttpOnly refresh cookie cannot live on a custom
// scheme) and what makes window.location.origin correct for the SPA, so that
// frontend/src/api/clients.ts needs no change and the plane needs no CORS.
//
// Build: `make desktop-build` (CGO on, this OS only — NOT part of the
// CGO_ENABLED=0 cross-compile in CROSS_PLATFORMS).
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	"github.com/wailsapp/wails/v2/pkg/options/mac"
	wailsruntime "github.com/wailsapp/wails/v2/pkg/runtime"

	"github.com/beardedparrott/orchicon/internal/controlport"
	"github.com/beardedparrott/orchicon/internal/desktop"
)

func main() {
	planeFlag := flag.String("plane", "", "plane URL (default: $ORCHICON_DESKTOP_PLANE_URL, else the dev control port)")
	flag.Parse()

	planeURL := *planeFlag
	if planeURL == "" {
		planeURL = os.Getenv("ORCHICON_DESKTOP_PLANE_URL")
	}
	if planeURL == "" {
		// The dev instance's control port, through the SHARED table — never a
		// hardcoded 8080 here (internal/controlport, asserted against the
		// installer's ORCHICON_CONTROL_PORT row).
		planeURL = fmt.Sprintf("http://127.0.0.1:%d", controlport.Defaults()["dev"])
	}

	shell, err := desktop.New(desktop.Config{PlaneURL: planeURL, Logf: log.Printf})
	if err != nil {
		log.Fatalf("orchicon-desktop: %v", err)
	}
	// Start REFUSES when the binary embeds no SPA — an empty window with no
	// explanation is the artefact that check exists to prevent.
	if err := shell.Start(); err != nil {
		log.Fatalf("orchicon-desktop: %v", err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = shell.Close(ctx)
	}()

	info := shell.Info()
	hostPort := controlport.HostPort(info.PlaneURL)
	geometryPath := desktop.ConfigPath()
	geom, ok := desktop.LoadGeometry(geometryPath)
	if !ok {
		geom = desktop.DefaultGeometry()
	}
	win := desktop.WindowOptionsFor(info, hostPort, geom)
	log.Printf("orchicon-desktop: %s → %s (UI %s, plane %s)", win.Title, shell.OriginURL(), info.AppTag, info.PlaneTag)

	app := &options.App{
		Title:     win.Title,
		Width:     win.W,
		Height:    win.H,
		MinWidth:  win.MinW,
		MinHeight: win.MinH,
		// THE SPA'S EXISTING TOP BAR IS THE APP CHROME. No Menu is set and the
		// shell draws no header: a second header above the SPA's is the most
		// likely way this drifts from the web app, and the content area must stay
		// visually identical to the browser's.
		AssetServer: &assetserver.Options{
			Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, shell.OriginURL()+r.URL.RequestURI(), http.StatusTemporaryRedirect)
			}),
		},
		// APPEARANCE: the window FRAME follows the OS; the CONTENT follows the
		// SPA's own theme store (default dark "Teal Depths"). The shell sets no
		// colour and mutates no class, so there is exactly one theme authority.
		Mac: &mac.Options{
			// The standard native title bar, deliberately: with a hidden-inset
			// title bar the content would start under the traffic lights and the
			// shell would have to draw a ~28px inset the SPA knows nothing about.
			TitleBar:   mac.TitleBarDefault(),
			Appearance: mac.DefaultAppearance, // follow the OS
		},
		OnDomReady: func(ctx context.Context) {
			restoreGeometry(ctx, geometryPath, geom)
		},
		OnBeforeClose: func(ctx context.Context) bool {
			saveGeometry(ctx, geometryPath)
			return false // allow the close
		},
	}
	if err := wails.Run(app); err != nil {
		log.Fatalf("orchicon-desktop: %v", err)
	}
}

// restoreGeometry applies remembered size and position, clamped to a monitor
// that actually exists — "restored only onto a visible monitor".
//
// Wails v2 reports screens by SIZE only (no origin), so each screen is treated
// as anchored at the origin: a saved position outside every screen's extent is
// therefore centred on the primary rather than silently restored onto a monitor
// that was unplugged.
func restoreGeometry(ctx context.Context, path string, saved desktop.Geometry) {
	monitors := make([]desktop.Rect, 0, 2)
	if screens, err := wailsruntime.ScreenGetAll(ctx); err == nil {
		primary := -1
		for _, s := range screens {
			if s.IsPrimary && primary < 0 {
				primary = len(monitors)
			}
			monitors = append(monitors, desktop.Rect{W: s.Size.Width, H: s.Size.Height})
		}
		if primary > 0 { // keep the primary first: ClampToMonitors centres on it
			monitors[0], monitors[primary] = monitors[primary], monitors[0]
		}
	}
	g := desktop.ClampToMonitors(saved, monitors)
	wailsruntime.WindowSetSize(ctx, g.W, g.H)
	if g.X != 0 || g.Y != 0 {
		wailsruntime.WindowSetPosition(ctx, g.X, g.Y)
	}
	if g.Maxed {
		wailsruntime.WindowMaximise(ctx)
	}
}

// saveGeometry persists the CURRENT window rect. Best-effort: a read-only
// config dir must not fail a window close.
func saveGeometry(ctx context.Context, path string) {
	w, h := wailsruntime.WindowGetSize(ctx)
	x, y := wailsruntime.WindowGetPosition(ctx)
	g := desktop.Geometry{X: x, Y: y, W: w, H: h, Maxed: wailsruntime.WindowIsMaximised(ctx)}
	if err := desktop.SaveGeometry(path, g); err != nil {
		log.Printf("orchicon-desktop: could not remember window geometry: %v", err)
		return
	}
	log.Printf("orchicon-desktop: remembered window geometry %+v", g)
}
