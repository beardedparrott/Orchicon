package aigateway

// discoverer_lazy.go — DISCOVERY THAT DOES NOT NEED A PLANE RESTART.
//
// REPORTED, twice, on two surfaces:
//
//	TUI:  "model discovery is not configured for this plane - set ORCHICON_OPENCODE_BIN or install
//	       opencode on PATH - then restart the plane."
//	GUI:  "This adapter needs the opencode CLI installed on the Orchicon host. Use the 'orchicon'
//	       adapter instead ... or install opencode and try again."
//
//	"... It should be auto discovering the location of opencode and automatically setting that.
//	 This should happen automatically when someone selects the opencode adapter in the TUI or the
//	 GUI (desktop or web)."
//
// THE OPERATOR IS RIGHT, AND THE OLD DESIGN COULD NOT DO IT. A ModelDiscoverer was constructed ONCE,
// during plane boot, from whatever the binary probe found at that instant. If the probe failed, the
// field stayed nil and EVERY subsequent request answered "not configured" for the life of the
// process. The message was therefore not "opencode is missing" but "the plane looked once, at
// startup, and will never look again" — which is invisible in the text and made a working machine
// look broken.
//
// Two independent reasons that first look can fail on a host where `opencode` runs fine:
//
//   - THE PLANE'S PATH IS A SNAPSHOT. internal/planecontrol builds the child environment from
//     os.Environ() at start time, so anything installed afterwards is invisible. (On Windows the
//     persisted PATH lives in the registry and is rebuilt at logon, which is why the operator can
//     "run it from anywhere" while the plane cannot see it at all — see internal/opencode's
//     registryPathDirs.)
//   - A PACKAGED INSTALL LANDS SOMEWHERE THE PROBE DID NOT KNOW. The candidate tables have been
//     widened for the common Windows locations.
//
// SO RESOLUTION IS NOW LAZY: when a request arrives and there is no discoverer, the plane resolves
// again and builds one. Selecting the opencode adapter IS the trigger, in every client — they all
// reach ListOpenCodeModels through this service, so the TUI, the desktop GUI and the web UI get the
// behaviour from one place rather than three.
//
// RE-PROBE ON EVERY REQUEST, CACHED ONCE FOUND. That is the operator's choice and it is the right
// trade: a failed resolution is a handful of os.Stat calls (no subprocess, no network), and the
// alternative — a cooldown — means "I installed it, I clicked, nothing happened" for some window of
// seconds, which is exactly the experience being fixed. Success is cached in the service so the
// probe cost is paid at most ONCE per process; after that the discoverer itself caches models with
// its own TTL.

import (
	"context"
	"log/slog"

	"github.com/beardedparrott/orchicon/internal/opencode"
)

// DiscovererFactory builds the pair of discoverers from a resolved opencode binary, or returns the
// reason it could not resolve one.
//
// IT IS A SEAM RATHER THAN AN IMPORT CYCLE. internal/server owns the boot-time wiring (it has the
// logger, the demand set and the context), and internal/aigateway must not import it. The plane
// installs this once at startup; the lazy path calls it again if the first attempt failed.
//
// BOTH RETURNS ARE DEREFERENCEABLE-OR-NIL on success/failure respectively: on error the discoverers
// are nil, and the caller reports the error rather than a half-built service.
type DiscovererFactory func() (*ModelDiscoverer, *MCPDiscoverer, error)

// SetDiscovererFactory installs the late resolver. Called once, beside NewService.
func (s *Service) SetDiscovererFactory(fn DiscovererFactory) {
	s.discovererMu.Lock()
	defer s.discovererMu.Unlock()
	s.discovererFactory = fn
}

// ensureDiscoverers returns the discoverers, resolving them on first need if boot did not.
//
// THE LOCK IS HELD ACROSS THE FACTORY CALL, which is deliberate: resolution reads the filesystem and
// may consult the Windows registry, and letting two concurrent model-tier requests both probe would
// double the work to learn the same answer. It is bounded and infrequent — once per process on
// success — so serialising it costs nothing that matters. A FAILED resolution is NOT cached, because
// the whole point is that the next request tries again.
func (s *Service) ensureDiscoverers() (*ModelDiscoverer, *MCPDiscoverer) {
	s.discovererMu.Lock()
	defer s.discovererMu.Unlock()
	if s.discoverer != nil {
		return s.discoverer, s.mcpDiscoverer
	}
	if s.discovererFactory == nil {
		return nil, nil
	}
	model, mcp, err := s.discovererFactory()
	if err != nil || model == nil {
		// NOT AN ERROR RETURN HERE. The caller answers with the actionable "not configured" message
		// it always did; this path only decides whether a retry can succeed. Logging every failed
		// re-probe would turn a missing binary into a log flood, so the factory reports its own
		// reason once (see internal/server's boot wiring).
		return nil, nil
	}
	s.discoverer = model
	s.mcpDiscoverer = mcp
	return s.discoverer, s.mcpDiscoverer
}

// BuildDiscoverers is the production factory body, shared with boot.
//
// ONE CONSTRUCTION PATH, ON PURPOSE. The boot wiring and the lazy path must produce the SAME
// discoverer or late resolution would behave differently from early resolution — a second copy of
// SetModelsArgs is precisely how a model list starts depending on whether the plane happened to
// start before or after the install. internal/server calls this at boot; the lazy path calls it
// through the factory.
//
// THE ARGV COMES FROM THE DETECTED API SURFACE, because the two opencode versions need different
// forms of the same command: 2.x starts a background server unless asked for --standalone, and 1.x
// needs --verbose for its model metadata. The rule lives with the surface table (internal/opencode)
// so it cannot be re-derived at a call site and drift.
func BuildDiscoverers(ctx context.Context, log *slog.Logger, bin string) (*ModelDiscoverer, *MCPDiscoverer) {
	model := NewModelDiscoverer(log, bin)
	model.SetModelsArgs(opencode.ModelsArgs(ctx, log, bin))
	return model, NewMCPDiscoverer(log, bin)
}
