package desktop

import (
	"context"
	"errors"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/beardedparrott/orchicon/internal/version"
)

var (
	errNoPlaneURL     = errors.New("desktop: no plane URL configured (set ORCHICON_DESKTOP_PLANE_URL or pass --plane)")
	errBadPlaneScheme = errors.New("desktop: plane URL must be http or https")
	errNoPlaneHost    = errors.New("desktop: plane URL has no host")
)

// Config is the shell's whole configuration. Deliberately tiny: the app is a
// thin client over a separately running plane, so there is nothing else to
// decide.
type Config struct {
	// PlaneURL is the plane this window points at, e.g. http://127.0.0.1:8080.
	PlaneURL string
	// Listen is the origin's bind address. Empty means 127.0.0.1:0 — an
	// ephemeral loopback port, which is the point: the webview gets a real http
	// origin without the shell competing for the plane's port.
	Listen string
	// FrontendFS overrides the embedded SPA. Tests inject a bundle; the desktop
	// binary never sets it.
	FrontendFS fs.FS
	// HTTPClient is used for the startup /versionz read. nil means the default.
	HTTPClient *http.Client
	// Logf is the shell's log sink (optional).
	Logf func(format string, args ...any)
	// ControlToken is the credential the SPA sends to the shell's own
	// plane-control endpoints. Generated per run by the caller; empty disables
	// the token check (tests that drive the endpoints directly).
	ControlToken string
	// PrefsPath is where the chosen instance is remembered. Empty means "do not
	// persist" (tests, and a host with no config dir).
	PrefsPath string
}

// Shell owns the loopback origin server: the embedded SPA plus the plane proxy.
//
// It is a plain net/http server on purpose — not a Wails asset server — so the
// transport can be tested, and driven live, without a webview in the loop.
type Shell struct {
	planeURL  *url.URL
	info      Info
	spaFS     fs.FS
	indexHTML []byte

	mu   sync.Mutex
	ln   net.Listener
	addr string
	srv  *http.Server

	// appTag is this binary's own version, captured at New so UsePlane can
	// rebuild Info without re-reading the (unchanging) build metadata.
	appTag string
	// control is the plane-control surface, nil on a shell that serves no
	// control endpoints (a test bundle, or a browser-shaped harness).
	control      Control
	controlToken string
	servers      Servers
	prefsPath    string

	logf func(format string, args ...any)
}

// New validates the plane URL, resolves the desktop Info (including the plane's
// own version) and prepares the embedded SPA.
//
// A plane that does not answer /versionz is NOT an error here: the app must open
// against a plane that is merely slow or not started yet, and it says so through
// PlaneTag: "unreachable" rather than refusing to run.
func New(cfg Config) (*Shell, error) {
	planeURL, err := planeURLFromString(cfg.PlaneURL)
	if err != nil {
		return nil, err
	}
	logf := cfg.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}

	spaFS := cfg.FrontendFS
	if spaFS == nil {
		spaFS, err = embeddedSPA()
		if err != nil {
			return nil, err
		}
	}

	planeTag, err := FetchPlaneTag(context.Background(), planeURL.String(), cfg.HTTPClient)
	if err != nil {
		logf("desktop: plane version unavailable: %v", err)
		planeTag = PlaneUnreachable
	}
	info := BuildInfo(planeURL.String(), version.Current().Tag, planeTag)

	indexHTML, err := readIndexHTML(spaFS, info, cfg.ControlToken)
	if err != nil {
		return nil, err
	}

	return &Shell{
		planeURL:     planeURL,
		info:         info,
		appTag:       version.Current().Tag,
		spaFS:        spaFS,
		indexHTML:    indexHTML,
		controlToken: cfg.ControlToken,
		prefsPath:    cfg.PrefsPath,
		logf:         logf,
	}, nil
}

// Start binds the origin and begins serving. It is idempotent-ish: calling it
// twice returns the existing address rather than binding a second port.
//
// THE SERVER'S TIMEOUTS ARE LOAD-BEARING. WriteTimeout is ZERO: the SPA reads a
// Connect server-stream (/orchicon.api.v1.ExecutionService/StreamExecutionEvents)
// for the life of an execution, and a non-zero write deadline silently truncates
// it mid-execution — the "streaming view that silently stops updating" failure.
// ReadHeaderTimeout stays finite so a half-open request cannot pin a connection
// forever, and IdleTimeout keeps an idle webview from holding sockets
// indefinitely.
func (s *Shell) Start() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.addr != "" {
		return nil
	}
	if len(s.indexHTML) == 0 {
		// FATAL, not a warning: an empty window with no explanation is the exact
		// artefact this check exists to prevent.
		return errors.New(noFrontendMessage)
	}
	// LOOPBACK ONLY, unconditionally. This origin serves the embedded SPA and
	// proxies the plane's API with no authentication of its own, so binding it
	// anywhere but the loopback address would hand the plane to the LAN. The port
	// is ephemeral so the shell never competes with the plane's own.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	mux := http.NewServeMux()
	spa := s.spaHandler()
	proxy := s.newProxy()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// ORDER IS THE CONTRACT, most specific first:
		//   1. our own control endpoints — never forwarded to the plane, or a
		//      plane route added later could shadow them;
		//   2. plane paths (the prefix table in proxy.go);
		//   3. the embedded SPA, which is the fallback everything else wants.
		// ONE DISPATCHER, LONGEST PREFIX FIRST — see surfaces.go. Listing the two calls here in a chosen
		// order is what produced the 404: controlPath is a PREFIX of serversPath, so the control surface
		// claimed every servers request and answered 404 for a path it does not implement.
		if s.handleOwnEndpoint(w, r) {
			return
		}
		if isPlanePath(r.URL.Path) {
			proxy.ServeHTTP(w, r)
			return
		}
		spa.ServeHTTP(w, r)
	})
	s.srv = &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      0, // see above: streaming must not be truncated
		IdleTimeout:       120 * time.Second,
	}
	s.ln = ln
	s.addr = ln.Addr().String()
	go func() {
		if err := s.srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.logMessage("desktop: origin server stopped: %v", err)
		}
	}()
	s.logMessage("desktop: SPA origin http://%s → plane %s", s.addr, s.planeURL)
	return nil
}

// Addr is the bound listen address ("" before Start).
func (s *Shell) Addr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.addr
}

// OriginURL is the http origin the webview must load. It is the ONLY origin the
// SPA ever sees: window.location.origin in the app is this address, which is why
// frontend/src/api/clients.ts needs no change.
func (s *Shell) OriginURL() string {
	addr := s.Addr()
	if addr == "" {
		return ""
	}
	return "http://" + addr
}

// binaryTag is this binary's own version, as captured at New.
func (s *Shell) binaryTag() string { return s.appTag }

// Info is the resolved desktop payload (plane URL, instance, versions).
func (s *Shell) Info() Info { return s.info }

// Close shuts the origin down.
func (s *Shell) Close(ctx context.Context) error {
	s.mu.Lock()
	srv := s.srv
	s.mu.Unlock()
	if srv == nil {
		return nil
	}
	return srv.Shutdown(ctx)
}

// logf is the shell's log sink, safe on a zero-value Shell.
func (s *Shell) logMessage(format string, args ...any) {
	if s.logf == nil {
		return
	}
	s.logf(format, args...)
}
