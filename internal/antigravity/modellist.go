package antigravity

// modellist.go — THE LIVE agy MODEL LISTING, behind a TTL cache.
//
// THERE IS NO `agy models` SUBCOMMAND (checked; do not invent one). agy's model catalogue is the
// `models.availableModels` array of a `session/new` RESULT — the verbatim shape is in
// internal/antigravity/testdata/acp/handshake_auth.jsonl:8, 27 models from agy 1.5.13. Listing it therefore
// needs the full handshake: initialize → authenticate → session/new → session/close, exactly the sequence
// internal/antigravity/sessionrun.go performs before a turn.
//
// THE CACHE IS NOT AN OPTIMISATION. A picker open is a single RPC but an operator opens it many times, and
// every miss would spawn an `agy` child and run an authenticated handshake — a process spawn per keystroke
// in the worst case, and each one a fresh plan-quota negotiation. The TTL mirrors
// orchicon.ClaudeCatalogModels' cache (internal/orchicon/claude_catalog.go): SUCCESS ONLY. An auth failure is
// never cached, because the operator's fix (signing in) must take effect on the next picker open, not after
// a TTL expires.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/beardedparrott/orchicon/internal/adapter"
	"github.com/beardedparrott/orchicon/internal/antigravity/acp"
)

// modelListTTL is how long a successful listing is served from memory.
const modelListTTL = 10 * time.Minute

// modelListTimeout bounds ONE handshake. A listing that cannot complete in this window is a handshake that
// is not going to complete; the caller falls back to the vendored snapshot rather than holding the picker's
// RPC open.
const modelListTimeout = 20 * time.Second

// ErrModelListAuthRequired is the sentinel behind every AuthRequiredError: the host cannot authenticate to
// agy, so there is no catalogue to list.
//
// IT IS A SENTINEL, NOT A STRING MATCH AT THE CALL SITE. The providers tier (internal/providers, which
// cannot import this package) maps `errors.Is(err, ErrModelListAuthRequired)` onto its own
// ErrAntigravityAuth, which handler.mapErr turns into connect.CodeFailedPrecondition carrying
// adapter.AntigravityAuthRequiredMessage.
var ErrModelListAuthRequired = errors.New(adapter.AntigravityAuthRequiredMessage)

// ErrModelListNotInstalled is the sentinel behind every NotInstalledError: there is no `agy` executable on
// this host at all.
//
// WHY IT IS NOT THE AUTH SENTINEL. It used to be. A missing binary was classified as an authentication
// failure, so the picker told an operator on a host with no `agy` to "run `agy` and sign in" — an
// instruction they could not carry out. The two are separate preconditions with separate fixes (install
// versus sign in), and the model tier's EMPTY state now says which one applies.
var ErrModelListNotInstalled = errors.New(adapter.AntigravityNotInstalledMessage)

// NotInstalledError reports that the model listing found no `agy` executable on the host.
//
// Error() returns adapter.AntigravityNotInstalledMessage VERBATIM for the same reason AuthRequiredError
// does: that string is what the operator reads in the picker's empty state.
type NotInstalledError struct {
	// Detail is the resolver's own text (which PATH was searched, a shim hint), for the log.
	Detail string
	// Cause is the resolver failure, for errors.Is/As through Unwrap.
	Cause error
}

func (e *NotInstalledError) Error() string { return adapter.AntigravityNotInstalledMessage }

func (e *NotInstalledError) Unwrap() error {
	if e.Cause != nil {
		return fmt.Errorf("%w: %w", ErrModelListNotInstalled, e.Cause)
	}
	return ErrModelListNotInstalled
}

// notInstalled builds the install instruction, keeping the resolver's text on the wrapped cause.
func notInstalled(detail string, cause error) *NotInstalledError {
	if cause != nil && detail == "" {
		detail = cause.Error()
	}
	return &NotInstalledError{Detail: detail, Cause: cause}
}

// AuthRequiredError reports that the model listing could not authenticate: agy is present and signed out.
//
// Error() returns adapter.AntigravityAuthRequiredMessage VERBATIM, because that string reaches the operator
// through connect.NewError as `[failed_precondition] <message>`. The transport detail lives in Detail, for
// the log.
type AuthRequiredError struct {
	Detail string
	Cause  error
}

func (e *AuthRequiredError) Error() string { return adapter.AntigravityAuthRequiredMessage }

func (e *AuthRequiredError) Unwrap() error {
	if e.Cause != nil {
		return fmt.Errorf("%w: %w", ErrModelListAuthRequired, e.Cause)
	}
	return ErrModelListAuthRequired
}

// authRequired builds the one actionable failure, classifying detail through the shared signal list first
// so a future agy wording does not silently become a generic error.
func authRequired(detail string, cause error) *AuthRequiredError {
	if mapped, ok := adapter.AntigravityAuthFailure(detail); ok {
		detail = mapped
	}
	return &AuthRequiredError{Detail: detail, Cause: cause}
}

// modelCacheEntry is one tenant's cached catalogue.
type modelCacheEntry struct {
	models  []acp.Model
	expires time.Time
}

// modelCache is the per-tenant listing cache (guarded; the picker RPCs are concurrent).
type modelCache struct {
	mu      sync.Mutex
	entries map[string]modelCacheEntry
}

// ListModels returns agy's model catalogue for one tenant, served from a 10-minute cache when warm.
//
// The models are agy's OWN entries: Model.ModelID is the bare id (`gemini-4-carbon-high`), which is exactly
// what a provider model row must carry — the picker composes `antigravity/google/<id>` itself, so returning
// a composed ref here would produce a 4-segment ref.
func (b *Bridge) ListModels(ctx context.Context, tenantID string) ([]acp.Model, error) {
	if models, ok := b.cachedModels(tenantID); ok {
		return models, nil
	}
	models, err := b.fetchModels(ctx, tenantID)
	if err != nil {
		// NEVER CACHED: an auth failure is the operator's to fix, and a stale cached failure would keep the
		// picker broken after they had fixed it.
		return nil, err
	}
	if len(models) == 0 {
		return nil, errors.New("antigravity: the agy handshake returned no models")
	}
	b.storeModels(tenantID, models)
	return models, nil
}

// cachedModels returns a warm, unexpired entry.
func (b *Bridge) cachedModels(tenantID string) ([]acp.Model, bool) {
	b.models.mu.Lock()
	defer b.models.mu.Unlock()
	entry, ok := b.models.entries[tenantID]
	if !ok || time.Now().After(entry.expires) {
		return nil, false
	}
	out := make([]acp.Model, len(entry.models))
	copy(out, entry.models)
	return out, true
}

// storeModels caches one successful listing.
func (b *Bridge) storeModels(tenantID string, models []acp.Model) {
	b.models.mu.Lock()
	defer b.models.mu.Unlock()
	if b.models.entries == nil {
		b.models.entries = make(map[string]modelCacheEntry, 1)
	}
	kept := make([]acp.Model, len(models))
	copy(kept, models)
	b.models.entries[tenantID] = modelCacheEntry{models: kept, expires: time.Now().Add(modelListTTL)}
}

// InvalidateModelCache drops one tenant's cached catalogue (nil/"" clears every tenant), so an operator who
// just signed in does not wait out the TTL.
func (b *Bridge) InvalidateModelCache(tenantID string) {
	b.models.mu.Lock()
	defer b.models.mu.Unlock()
	if tenantID == "" {
		b.models.entries = nil
		return
	}
	delete(b.models.entries, tenantID)
}

// fetchModels runs one initialize → authenticate → session/new → session/close handshake and returns the
// catalogue agy reported.
//
// EVERY STEP CAN PRODUCE THE ACTIONABLE AUTH FAILURE, and each one is mapped through the same classifier:
// a missing binary, a settings resolution that cannot supply the API key its method needs, an authenticate
// refusal, and agy's own -32000 "Authentication required" on session/new are the same operator-facing
// problem with the same fix.
func (b *Bridge) fetchModels(ctx context.Context, tenantID string) ([]acp.Model, error) {
	ctx, cancel := context.WithTimeout(ctx, modelListTimeout)
	defer cancel()

	settings, apiKey, err := b.resolveSettings(ctx, tenantID)
	if err != nil {
		if _, ok := adapter.AntigravityAuthFailure(err.Error()); ok {
			return nil, authRequired(err.Error(), err)
		}
		return nil, fmt.Errorf("antigravity: resolve tenant settings for the model listing: %w", err)
	}
	bin, binErr := b.resolveBinary()
	if binErr != nil {
		// NO agy ON THIS HOST IS NOT A SIGN-IN PROBLEM. It is the install precondition, and it gets its
		// own sentence: telling an operator to "run `agy`" on a host with no `agy` is unfollowable.
		return nil, notInstalled(binErr.Error(), binErr)
	}
	spec, err := BuildSpawnSpec(bin, settings, apiKey, os.Environ())
	if err != nil {
		// A gemini-api-key method with no key configured: auth-shaped by construction.
		return nil, authRequired(err.Error(), err)
	}
	if err := assertNoBypass(spec.Argv); err != nil {
		// Not auth: a spec carrying a bypass flag is a bug, and it must not be reported as "sign in".
		return nil, err
	}

	procSpec := spec.ProcSpec()
	procSpec.Cwd = os.TempDir()

	client, err := acp.Start(ctx, acp.Config{
		Factory:    b.modelListFactory(),
		Launch:     acp.LaunchOptions{Binary: spec.Argv[0]},
		Spec:       procSpec,
		AuthMethod: spec.AuthMethod,
		ClientName: "orchicon-model-list",
		// A nil Handler REJECTS every permission request — the fail-closed default. A listing issues no
		// tools, so nothing should ever ask.
		ClientVersion: clientVersion,
		Logger:        b.log,
	})
	if err != nil {
		if _, ok := adapter.AntigravityAuthFailure(err.Error()); ok {
			return nil, authRequired(err.Error(), err)
		}
		return nil, fmt.Errorf("antigravity: spawn agy for the model listing: %w", err)
	}
	defer func() {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), acp.DefaultShutdownGrace+5*time.Second)
		defer closeCancel()
		_ = client.Close(closeCtx)
	}()

	if _, err := client.Initialize(ctx); err != nil {
		if _, ok := adapter.AntigravityAuthFailure(err.Error()); ok {
			return nil, authRequired(err.Error(), err)
		}
		return nil, fmt.Errorf("antigravity: initialize failed for the model listing: %w", err)
	}
	// AUTH IS EXPLICIT HERE TOO: agy defers its credential check to the first model call, so a listing that
	// skipped authenticate would report a catalogue on a signed-out host — the exact lie this tier must not
	// tell. Authenticate is what makes the signed-out state observable at the picker's own latency.
	if err := client.Authenticate(ctx, spec.AuthMethod); err != nil {
		var authErr *acp.AuthError
		if errors.As(err, &authErr) {
			return nil, authRequired(authErr.Detail, err)
		}
		return nil, authRequired(err.Error(), err)
	}

	sess, state, err := client.NewSession(ctx, acp.NewSessionParams{Cwd: os.TempDir(), MCPServers: []acp.MCPServer{}})
	if err != nil {
		if _, ok := adapter.AntigravityAuthFailure(err.Error()); ok {
			return nil, authRequired(err.Error(), err)
		}
		return nil, fmt.Errorf("antigravity: session/new failed for the model listing: %w", err)
	}
	models := append([]acp.Model(nil), state.Models...)
	// CLOSE THE SESSION EXPLICITLY, on a context the caller's deadline cannot have cancelled: agy keeps the
	// child's session registry until it sees session/close, and leaking one per picker open would grow the
	// child's state for as long as it lives.
	closeCtx, closeCancel := context.WithTimeout(context.Background(), acp.DefaultShutdownGrace)
	defer closeCancel()
	if err := sess.Close(closeCtx); err != nil {
		b.log.Warn("antigravity: session/close failed after a model listing", "tenant", tenantID, "error", err)
	}
	return models, nil
}

// ListModelRows is the providers-facing projection of ListModels: the bare agy model ids and their display
// names, in agy's own order, de-duplicated.
//
// IT EXISTS SO THE PLANE CAN WIRE THE LISTER WITHOUT THIS PACKAGE IMPORTING internal/providers (that
// direction would be a cycle). internal/server/server.go maps these pairs into providers.AgyModel.
func (b *Bridge) ListModelRows(ctx context.Context, tenantID string) ([][2]string, error) {
	models, err := b.ListModels(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]bool, len(models))
	out := make([][2]string, 0, len(models))
	for _, m := range models {
		if m.ModelID == "" || seen[m.ModelID] {
			continue
		}
		seen[m.ModelID] = true
		out = append(out, [2]string{m.ModelID, m.Name})
	}
	return out, nil
}

// modelListFactory picks the transport ONE model listing runs on.
//
// IT IS NOT spawnFactory(), AND THAT IS THE WHOLE POINT. spawnFactory() prefers the CONTAINER transport
// whenever the bridge holds a runtime client, because a WORKER's child must run inside its run's container.
// A model listing has no run: it is a picker RPC, not a dispatch, so there is no run id to lease a container
// by and the container factory refuses it outright ("no workflow run id for container spawn"). MEASURED on a
// local-mode plane with a wired runtime client: every picker open logged that failure and silently fell back
// to the vendored snapshot, so the operator never saw the live catalogue even though `agy` was installed and
// signed in. On a plane WITHOUT a runtime client the bug is invisible, which is why it has to be pinned by a
// test rather than noticed.
//
// A listing therefore runs on the LOCAL transport — the host's own `agy`, which is exactly the binary whose
// catalogue the picker is offering and whose credentials the operator signed in with. An explicit
// factoryOverride (tests) still wins, so the existing fakes drive this path unchanged.
func (b *Bridge) modelListFactory() acp.ProcFactory {
	if b.factoryOverride != nil {
		return b.factoryOverride
	}
	return acp.LocalFactory{}.NewProc
}
