//go:build orchicon_desktop

// orchicon-desktop is the thin Wails entry point: it owns NO logic.
package main

import (
	"context"
	_ "embed"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	"github.com/wailsapp/wails/v2/pkg/options/linux"
	"github.com/wailsapp/wails/v2/pkg/options/mac"
	wailsruntime "github.com/wailsapp/wails/v2/pkg/runtime"

	"github.com/beardedparrott/orchicon/internal/controlport"
	"github.com/beardedparrott/orchicon/internal/desktop"
	"github.com/beardedparrott/orchicon/internal/instancectl"
	"github.com/beardedparrott/orchicon/internal/planebind"
)

// appIcon is THE WINDOW ICON, and it is a DIFFERENT thing from the packaging icon.
//
// build/appicon.png feeds the PACKAGER — Wails turns it into the .ico for Windows,
// the .icns for macOS and the .desktop/AppImage art for Linux. It does NOT reach a
// running window: on Linux the window's icon comes from options.Linux.Icon, a []byte
// the APP must supply (internal/frontend/desktop/linux/window.go SetWindowIcon). We
// set neither, so the window had no icon and the desktop environment drew its own
// fallback — the generic Wails-yellow square the operator saw on the task bar, while
// the .deb produced by a package build already carried our mark.
//
// Embedding it here means the icon is IN the binary, so `make desktop-build` (which
// is `go build`, not `wails build`, and therefore never runs the packager) still
// ships a branded window.
//
//go:embed build/appicon.png
var appIcon []byte

func main() {
	planeFlag := flag.String("plane", "", "plane URL (default: $ORCHICON_DESKTOP_PLANE_URL, else --instance / the remembered instance)")
	instanceFlag := flag.String("instance", "", "which instance to connect to: dev, prod or test (default: $ORCHICON_DESKTOP_INSTANCE, else the remembered choice, else dev)")
	// ROUTE is where the window OPENS.
	routeFlag := flag.String("route", "", "page to open on launch, e.g. /manage-server (default: the app's own start page)")
	// A REPLACEMENT PROCESS WAITS FOR ITS PREDECESSOR TO EXIT BEFORE IT BEGINS — see internal/desktop
	// relaunch.go.
	awaitPID, restArgv, isReplacement := desktop.ParseAwaitPID(os.Args[1:])
	if isReplacement {
		os.Args = append([]string{os.Args[0]}, restArgv...)
		if !desktop.WaitForExit(awaitPID, 20*time.Second, processAlive, nil) {
			// THE PREDECESSOR IS STILL RUNNING.
			log.Printf("orchicon-desktop: the previous window (pid %d) did not exit; not opening a second one", awaitPID)
			return
		}
	}
	flag.Parse()

	// WHICH PLANE, IN PRECEDENCE ORDER — and every step of it is a way to say so, because there was
	// previously no way at all and the app silently picked dev.
	planeURL := *planeFlag
	if planeURL == "" {
		planeURL = os.Getenv("ORCHICON_DESKTOP_PLANE_URL")
	}
	instance := *instanceFlag
	if planeURL == "" && instance == "" {
		instance = os.Getenv("ORCHICON_DESKTOP_INSTANCE")
	}
	if planeURL == "" && instance == "" {
		instance = desktop.LoadChosenInstance(desktop.PrefsPath())
	}
	if planeURL == "" {
		if instance == "" {
			instance = desktop.DefaultInstance
		}
		port, ok := controlport.Defaults()[instance]
		if !ok {
			log.Fatalf("orchicon-desktop: unknown instance %q — expected dev, prod or test", instance)
		}
		// Through the SHARED table — never a hardcoded 8080 here (internal/controlport, asserted against
		// the installer's ORCHICON_CONTROL_PORT row).
		planeURL = fmt.Sprintf("http://127.0.0.1:%d", port)
	}

	// ONE token, generated once, used for both the injected payload and the endpoint check — generating
	// it twice would hand the SPA a credential the endpoints do not accept.
	controlToken := desktop.NewControlToken()

	shell, err := desktop.New(desktop.Config{
		PlaneURL: planeURL,
		Logf:     log.Printf,
		// Per-run, and injected into the SPA only. See control.go for exactly what
		// this does and does not defend against.
		ControlToken: controlToken,
		PrefsPath:    desktop.PrefsPath(),
	})
	if err != nil {
		log.Fatalf("orchicon-desktop: %v", err)
	}
	// Start REFUSES when the binary embeds no SPA — an empty window with no explanation is the artefact
	// that check exists to prevent.
	shell.SetControl(&planebind.Service{}, controlToken)
	// THE SERVERS SURFACE (design of record, phase 1): list / install / uninstall / start / stop /
	// restart / setup-progress, all delegating to internal/instancectl.
	shell.SetServers(instancectl.NewService(nil))

	// THE RESTART HOOK — what makes "Connect" actually change servers.
	shell.SetRestart(func(instance, route string) error {
		self, err := os.Executable()
		if err != nil {
			return err
		}
		pid, err := desktop.StartReplacement(self, os.Getpid(), instance, route)
		if err != nil {
			return err
		}
		log.Printf("orchicon-desktop: restarting as pid %d on instance %s (%s)", pid, instance, route)
		// EXIT AFTER A SHORT DELAY, so the HTTP response that triggered this is flushed to the page before
		// the origin socket goes away.
		go func() {
			time.Sleep(250 * time.Millisecond)
			wailsruntime.Quit(context.Background())
		}()
		return nil
	})

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
		// THE SPA'S EXISTING TOP BAR IS THE APP CHROME.
		AssetServer: &assetserver.Options{
			// THE WINDOW'S FIRST DOCUMENT IS A NAVIGATION, NOT A REDIRECT.
			Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/html; charset=utf-8")
				w.Header().Set("Cache-Control", "no-store")
				// --route decides the OPENING PAGE (D7).
				_, _ = w.Write([]byte(desktop.BootstrapHTML(shell.OriginURL() + desktop.SafeRoute(*routeFlag))))
			}),
		},
		// THE PLANE CONTROL BINDING.
		Bind: []interface{}{&planebind.Service{}},
		// APPEARANCE: the window FRAME follows the OS; the CONTENT follows the SPA's own theme store
		// (default dark "Teal Depths").
		Linux: &linux.Options{
			Icon: appIcon,
			// EXPLICIT, because passing a non-nil Linux options CHANGES THE DEFAULT: Wails documents that a nil
			// options.Linux sets WebviewGpuPolicy to Never (wails#2977).
			WebviewGpuPolicy: linux.WebviewGpuPolicyNever,
			// Window grouping with the installed .desktop file (deploy/desktop/linux/orchicon-desktop.desktop).
			ProgramName: "orchicon-desktop",
		},
		Mac: &mac.Options{
			// The standard native title bar, deliberately: with a hidden-inset title bar the content would
			// start under the traffic lights and the shell would have to draw a ~28px inset the SPA knows.
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

// restoreGeometry applies remembered size and position, clamped to a monitor that actually exists —
// "restored only onto a visible monitor".
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

// processAlive reports whether pid is still running.
