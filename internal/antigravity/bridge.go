package antigravity

// bridge.go — the antigravity (agy) adapter bridge, which is a PLACEHOLDER today.
//
// WHY A STUB SHIP AT ALL, instead of simply not registering the kind: a kind that is registered
// nowhere produces `no ready adapters of kind "antigravity"` — the same opaque failure the platform
// has now hit three times for opencode, orchicon and claude. Registering the kind with a bridge that
// REFUSES CLEANLY turns that into a named, actionable error ("the kind is known, its session
// behaviour is not built yet"), so children 5/6 fill in behaviour behind a boundary that already
// exists rather than building the boundary too.

import (
	"context"
	"errors"
	"log/slog"

	"github.com/beardedparrott/orchicon/internal/adapter"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/runtime"
	"github.com/beardedparrott/orchicon/internal/scheduler"
)

// ErrNotImplemented is the stub's single refusal. It names the kind AND says the kind is registered,
// because "not implemented" alone reads like "unknown kind" to an operator debugging a dispatch.
var ErrNotImplemented = errors.New(`antigravity bridge not yet implemented (kind "antigravity" is registered and ` +
	`has a ready adapter row, but its session behaviour is not built yet — agy session support is the ` +
	`next step; the adapter kind, its model refs (antigravity/google/<model>) and its auth-failure ` +
	`mapping are in place)`)

// Bridge is the Antigravity (agy) adapter. It holds the runtime client so the eventual session path
// has the same injection seam every other adapter uses, and so the server wiring (which passes it) is
// already correct when that path lands.
type Bridge struct {
	log *slog.Logger
	rt  *runtime.Client
}

// Compile-time assertions: the bridge must satisfy the two interfaces the dispatcher and the session
// ownership resolution read. A stub that silently stopped satisfying them would compile fine and
// break routing at runtime.
var (
	_ scheduler.AdapterBridge    = (*Bridge)(nil)
	_ scheduler.SessionOwnerKind = (*Bridge)(nil)
)

// New builds an Antigravity bridge.
func New(log *slog.Logger) *Bridge {
	if log == nil {
		log = slog.Default()
	}
	return &Bridge{log: log}
}

// SetRuntimeClient injects the workflow runtime daemon client (concrete setter, mirroring
// opencode's and claude's). Nil keeps the bridge in-process.
func (b *Bridge) SetRuntimeClient(rt *runtime.Client) { b.rt = rt }

// SPAWNING RULE for the session path that replaces this stub: build the child invocation with
// BuildSpawnSpec (procspec.go), never by appending flags at the call site. It is the one place the
// tenant's auth method, the GEMINI_API_KEY environment variable and the --disable-telemetry policy
// are applied, and its String/LogValue redact the key.
//
// Start implements scheduler.AdapterBridge. It REFUSES with ErrNotImplemented: no shim, no network
// call, no partial session. A half-started session would be indistinguishable from a working one to
// the reconciler and would burn a run's budget before failing.
func (b *Bridge) Start(ctx context.Context, exec db.ExecutionRow, manifest scheduler.ExecutionManifest, callbacks scheduler.ExecutionCallbacks) error {
	b.log.Warn("antigravity dispatch refused: bridge is a stub",
		"execution", exec.ID, "worker", exec.WorkerID, "tenant", exec.TenantID)
	return ErrNotImplemented
}

// SessionOwnerKind implements scheduler.SessionOwnerKind.
func (b *Bridge) SessionOwnerKind() string { return adapter.KindAntigravity }
