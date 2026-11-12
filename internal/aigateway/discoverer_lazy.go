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
type DiscovererFactory func() (*ModelDiscoverer, *MCPDiscoverer, error)

// SetDiscovererFactory installs the late resolver. Called once, beside NewService.
func (s *Service) SetDiscovererFactory(fn DiscovererFactory) {
	s.discovererMu.Lock()
	defer s.discovererMu.Unlock()
	s.discovererFactory = fn
}

// ensureDiscoverers returns the discoverers, resolving them on first need if boot did not.
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
		// NOT AN ERROR RETURN HERE.
		return nil, nil
	}
	s.discoverer = model
	s.mcpDiscoverer = mcp
	return s.discoverer, s.mcpDiscoverer
}

// BuildDiscoverers is the production factory body, shared with boot.
func BuildDiscoverers(ctx context.Context, log *slog.Logger, bin string) (*ModelDiscoverer, *MCPDiscoverer) {
	model := NewModelDiscoverer(log, bin)
	model.SetModelsArgs(opencode.ModelsArgs(ctx, log, bin))
	return model, NewMCPDiscoverer(log, bin)
}
