//go:build orchicon_desktop

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
	// ROUTE is where the window OPENS. It exists so an installer can land a first run on the page that
	// finishes setting up (design of record, D7): the MSI's Finish-page checkbox launches
	// `orchicon-desktop.exe --route /manage-server`, and the app opens there instead of the home page.
	//
	// It is a plain route path, validated to be one of ours — a launch argument is not a channel for
	// arbitrary navigation, and a typo must land on the default page rather than a blank window.
	routeFlag := flag.String("route", "", "page to open on launch, e.g. /manage-server (default: the app's own start page)")
	// A REPLACEMENT PROCESS WAITS FOR ITS PREDECESSOR TO EXIT BEFORE IT BEGINS — see internal/desktop
	// relaunch.go. The internal flag is consumed HERE, before flag.Parse, or it would be reported as an
	// undefined flag and the app would exit without ever opening a window.
	awaitPID, restArgv, isReplacement := desktop.ParseAwaitPID(os.Args[1:])
	if isReplacement {
		os.Args = append([]string{os.Args[0]}, restArgv...)
		if !desktop.WaitForExit(awaitPID, 20*time.Second, processAlive, nil) {
			// THE PREDECESSOR IS STILL RUNNING. Two windows are worse than a stale one, so this process
			// gives up rather than fighting for the single-instance slot — and says so in the log, because
			// a silent exit would look like "Connect did nothing".
			log.Printf("orchicon-desktop: the previous window (pid %d) did not exit; not opening a second one", awaitPID)
			return
		}
	}
	flag.Parse()

	// WHICH PLANE, IN PRECEDENCE ORDER — and every step of it is a way to say so,
	// because there was previously no way at all and the app silently picked dev.
	//
	//   1. --plane URL   the explicit escape hatch (a remote or non-standard plane)
	//   2. --instance    an instance name, resolved through the SHARED port table
	//   3. $ORCHICON_DESKTOP_INSTANCE
	//   4. the last instance chosen in the app (prefs.json)
	//   5. dev
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
		// Through the SHARED table — never a hardcoded 8080 here
		// (internal/controlport, asserted against the installer's
		// ORCHICON_CONTROL_PORT row).
		planeURL = fmt.Sprintf("http://127.0.0.1:%d", port)
	}

	// ONE token, generated once, used for both the injected payload and the
	// endpoint check — generating it twice would hand the SPA a credential the
	// endpoints do not accept.
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
	// Start REFUSES when the binary embeds no SPA — an empty window with no
	// explanation is the artefact that check exists to prevent.
	// THE PLANE-CONTROL SURFACE. Served from the shell's own origin (see
	// control.go for why it cannot be a Wails binding), delegating straight into
	// internal/planebind — the same package the CLI and the TUI drive.
	shell.SetControl(&planebind.Service{}, controlToken)
	// THE SERVERS SURFACE (design of record, phase 1): list / install / uninstall / start / stop / restart /
	// setup-progress, all delegating to internal/instancectl — which is the thin client of the CLI, so the
	// app, the TUI and `orchicon` cannot disagree about what an instance is.
	//
	// A nil writer on purpose: the app renders progress from the setup RECORD, not from this process's
	// stdout, and a detached install has no stdout here at all.
	shell.SetServers(instancectl.NewService(nil))

	// THE RESTART HOOK — what makes "Connect" actually change servers.
	//
	// REPORTED: "When you connect to another instance, the top menu disappears and you are left on the manage
	// server screen. If you close the desktop app and re-open, THEN you are connected to the other instance.
	// We may want to build in an ability to just auto restart the desktop app once someone hits connect."
	//
	// Yes — and that is not a workaround, it is the correct model. A window is initialised from the injected
	// payload, the proxy target, the version tags and a session store bound to the OLD plane's database. A
	// reload rebuilds only what the document carries, which is why the app arrives half-switched. Restarting
	// rebuilds all of it, by construction, and cannot miss state added later.
	//
	// THE REPLACEMENT WAITS FOR THIS PROCESS TO EXIT (--await-pid), so the two windows never coexist and the
	// new one starts from exactly the state a hand restart would.
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
		// the origin socket goes away. Exit immediately and the operator sees "Failed to fetch" on a switch
		// that in fact succeeded — the same dishonest error this whole page is written to avoid.
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
		// THE SPA'S EXISTING TOP BAR IS THE APP CHROME. No Menu is set and the
		// shell draws no header: a second header above the SPA's is the most
		// likely way this drifts from the web app, and the content area must stay
		// visually identical to the browser's.
		AssetServer: &assetserver.Options{
			// THE WINDOW'S FIRST DOCUMENT IS A NAVIGATION, NOT A REDIRECT.
			//
			// This used to answer `http.Redirect(..., StatusTemporaryRedirect)`.
			// WebKit's custom-scheme handler cannot turn a 3xx into a navigation:
			// it renders the response BODY instead, so the window opened as a white
			// page containing Go's own link text — "temporary redirect" — which is
			// precisely what the operator reported.
			//
			// bootstrapHTML is a page that navigates itself to the shell's real http
			// origin. That navigation the webview performs for real, on every
			// WebKitGTK version, and it is where the SPA must live anyway: the
			// HttpOnly refresh cookie needs an http origin and a Connect
			// server-stream must not be buffered through a scheme handler.
			Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/html; charset=utf-8")
				w.Header().Set("Cache-Control", "no-store")
				// --route decides the OPENING PAGE (D7). It is validated to a known app route, so a
				// launch argument cannot become an open redirect; an unrecognised value falls back to
				// the app's own start page rather than to a blank window.
				_, _ = w.Write([]byte(desktop.BootstrapHTML(shell.OriginURL() + desktop.SafeRoute(*routeFlag))))
			}),
		},
		// THE PLANE CONTROL BINDING. Manage Server's start/stop/restart reach
		// internal/planecontrol through this bound service — LOCAL invocation, never
		// the plane's API (there is no lifecycle RPC, and internal/api must not gain
		// one). Without this line the page renders actions it cannot perform, which
		// is why internal/planebind's wiring test is red on a shell that omits it.
		Bind: []interface{}{&planebind.Service{}},
		// APPEARANCE: the window FRAME follows the OS; the CONTENT follows the
		// SPA's own theme store (default dark "Teal Depths"). The shell sets no
		// colour and mutates no class, so there is exactly one theme authority.
		Linux: &linux.Options{
			Icon: appIcon,
			// EXPLICIT, because passing a non-nil Linux options CHANGES THE DEFAULT:
			// Wails documents that a nil options.Linux sets WebviewGpuPolicy to
			// Never (wails#2977). Adding an icon must not silently turn hardware
			// acceleration back on — that is a rendering change on a machine where
			// the safe default was deliberate.
			WebviewGpuPolicy: linux.WebviewGpuPolicyNever,
			// Window grouping with the installed .desktop file
			// (deploy/desktop/linux/orchicon-desktop.desktop). Without it the window
			// manager matches on the executable name instead.
			ProgramName: "orchicon-desktop",
		},
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

// processAlive reports whether pid is still running.
//
// SIGNAL 0 ON UNIX AND OpenProcess ON WINDOWS, in their own files — see alive_*.go. It exists so a
// replacement process can wait for its predecessor deterministically instead of sleeping for a guessed
// interval, which is how a self-restart ends up showing two windows.
