package scheduler

import (
	"errors"
	"fmt"
	"sort"
	"sync"
)

// ErrAdapterDisabled is the sentinel Resolve returns for an
// administratively disabled adapter kind. It is PERMANENT by definition:
// retrying cannot re-enable a kind, so the TaskReconciler blocks the work
// item with the reason instead of requeueing it (AC 3 — a loud failure, not
// a silent requeue loop). Deliberately distinct from the transient
// adapter-unreachable failure that keeps the requeue-with-backoff path.
var ErrAdapterDisabled = errors.New("adapter disabled")

// ErrAdapterKindUnregistered is the sentinel Resolve returns when no bridge
// is registered for a kind. A DECLARED kind that is not dispatcher-
// registered (today: "claude", a declared kind in
// internal/adapter/providers.go) resolves here — also permanent, because no
// retry can register a bridge. Callers must fail loudly rather than falling
// back to some other adapter silently.
var ErrAdapterKindUnregistered = errors.New("no adapter bridge registered")

// Dispatcher routes executions to the AdapterBridge for the adapter kind
// parsed from the execution's model_ref (adapter.ParseModelRef(ref).Adapter).
// It is the shared routing substrate that makes future adapters pluggable:
// a new adapter registers itself under its kind at construction time and
// the TaskReconciler + server RPC paths resolve the bridge per execution
// without ever referencing a concrete adapter type.
//
// Registration happens at server construction (startup, single goroutine);
// Resolve happens at dispatch time (possibly concurrent), so the registry
// is mutex-guarded.
type Dispatcher struct {
	mu      sync.RWMutex
	bridges map[string]AdapterBridge
	// disabled holds adapter kinds administratively switched OFF for this
	// plane (per-adapter enable/disable, AC 3). A disabled kind stays
	// REGISTERED — disabling is a switch, never a deregistration — so
	// Resolve can name the kind and say exactly why it will not dispatch,
	// instead of falling back to another adapter silently.
	disabled map[string]struct{}
	// diffLedger is the installed diff-ledger callback pair, applied to every
	// bridge at registration (and to those already registered). Nil until a
	// caller installs one, so a Dispatcher used without a ledger — every test
	// that does not care about diffs — behaves exactly as before.
	diffLedger *diffLedgerHooks
}

// NewDispatcher creates an empty Dispatcher. Adapters register via
// Register before the reconciler loop starts.
func NewDispatcher() *Dispatcher {
	return &Dispatcher{
		bridges:  make(map[string]AdapterBridge),
		disabled: make(map[string]struct{}),
	}
}

// Disable marks an adapter kind administratively disabled for this plane.
// Every later Resolve of that kind fails with ErrAdapterDisabled, which the
// TaskReconciler turns into a LOUD permanent dispatch failure (the work item
// is blocked with the reason) rather than a silent requeue. Idempotent and
// safe to call at runtime; an empty kind is ignored (it can never resolve
// anyway). Seeded at server construction from
// ORCHICON_DISABLED_ADAPTER_KINDS.
func (d *Dispatcher) Disable(kind string) {
	if kind == "" {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.disabled == nil {
		d.disabled = make(map[string]struct{})
	}
	d.disabled[kind] = struct{}{}
}

// DisabledKinds returns the administratively disabled kinds, sorted. An
// empty Dispatcher (or one with nothing disabled) yields an empty non-nil
// slice.
func (d *Dispatcher) DisabledKinds() []string {
	d.mu.RLock()
	defer d.mu.RUnlock()
	out := make([]string, 0, len(d.disabled))
	for k := range d.disabled {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Register associates the bridge with the given adapter kind (e.g.
// "opencode"). Registering the same kind twice overwrites the previous
// bridge (last registration wins) — it never panics. An empty kind is
// rejected: it could never be resolved (ParseModelRef never yields an
// empty Adapter), so registering it is a programming error surfaced
// loudly at startup.
//
// IT ALSO APPLIES THE DIFF-LEDGER SINK, when one has been installed and the
// bridge can take it. That is what makes diff support arrive BY REGISTERING
// rather than by being remembered: the hook used to be set on each bridge by
// hand in server construction, and the Ask half was set on exactly ONE of the
// three Ask-capable adapters — so `opencode` and `claude` conversations showed
// no files in the diff pane while the native one worked. A fourth adapter would
// have inherited the same gap.
func (d *Dispatcher) Register(kind string, bridge AdapterBridge) {
	if kind == "" {
		panic("scheduler: Dispatcher.Register with empty adapter kind")
	}
	if bridge == nil {
		panic("scheduler: Dispatcher.Register with nil bridge")
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.bridges[kind] = bridge
	// A bridge that cannot ledger simply does not implement DiffLedgerSink —
	// the same optional-capability discipline as MessageInjector/Aborter, and
	// never a panic.
	if d.diffLedger != nil {
		if sink, ok := bridge.(DiffLedgerSink); ok {
			sink.SetDiffLedger(d.diffLedger.execution, d.diffLedger.ask)
		}
	}
}

// SetDiffLedger installs the diff-ledger callbacks applied to EVERY bridge from
// now on, and to every bridge already registered.
//
// BOTH HALVES ARE APPLIED IN ONE PLACE, which is the fix rather than a
// convenience: the previous per-bridge setters let the execution half and the
// Ask half drift, and the Ask half was missing for two of the three Ask-capable
// adapters. A caller that wants only one population passes nil for the other.
//
// Applying to already-registered bridges as well means call order does not
// matter — a server that wires the ledger before or after Register gets the same
// result, so a future adapter cannot be silently left out by registering first.
func (d *Dispatcher) SetDiffLedger(execution, ask DiffLedgerFunc) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.diffLedger = &diffLedgerHooks{execution: execution, ask: ask}
	for _, b := range d.bridges {
		if sink, ok := b.(DiffLedgerSink); ok {
			sink.SetDiffLedger(execution, ask)
		}
	}
}

// diffLedgerHooks holds the installed callbacks. A small struct rather than two
// fields so the nil test is ONE question ("has a ledger been installed?")
// instead of two that could disagree.
type diffLedgerHooks struct {
	execution DiffLedgerFunc
	ask       DiffLedgerFunc
}

// DiffLedgerCapable reports whether the bridge registered for kind can accept a
// diff ledger. It exists so a test — and an operator-facing diagnostic — can ask
// "does this adapter support the diff pane?" without type-asserting a bridge
// itself, and so the answer is visible for an adapter nobody thought to check.
func (d *Dispatcher) DiffLedgerCapable(kind string) bool {
	d.mu.RLock()
	defer d.mu.RUnlock()
	b, ok := d.bridges[kind]
	if !ok {
		return false
	}
	_, capable := b.(DiffLedgerSink)
	return capable
}

// Resolve returns the AdapterBridge registered for the given adapter
// kind. An unknown kind yields an actionable error (it names the kind and
// the registered kinds) — never a panic. The caller (TaskReconciler or a
// server RPC path) surfaces that error as an execution failure / RPC
// error so an operator knows exactly which adapter kind is missing.
func (d *Dispatcher) Resolve(kind string) (AdapterBridge, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	if kind == "" {
		return nil, fmt.Errorf("no adapter kind specified (empty model_ref adapter segment) — cannot resolve a bridge")
	}
	if _, off := d.disabled[kind]; off {
		return nil, fmt.Errorf("%w: adapter kind %q is disabled — re-enable it to dispatch (configured via ORCHICON_DISABLED_ADAPTER_KINDS)", ErrAdapterDisabled, kind)
	}
	b, ok := d.bridges[kind]
	if !ok {
		return nil, fmt.Errorf("%w for kind %q — register it at server construction or fix the worker's model_ref (registered kinds: %s)", ErrAdapterKindUnregistered, kind, d.kindsLocked())
	}
	return b, nil
}

// Kinds returns the registered adapter kinds, sorted, deduped (map keys
// are unique by construction). It is the public enumeration surface the
// model picker's adapter bubble tier consumes (ADR-0004 D1): a new
// adapter appears in the picker automatically once it registers here.
// An empty Dispatcher yields an empty (non-nil) slice.
func (d *Dispatcher) Kinds() []string {
	d.mu.RLock()
	defer d.mu.RUnlock()
	kinds := make([]string, 0, len(d.bridges))
	for k := range d.bridges {
		kinds = append(kinds, k)
	}
	sort.Strings(kinds)
	return kinds
}

// ChatKinds returns the registered adapter kinds whose bridge implements the
// ChatTurnClient (Ask chat) capability. It is the Kinds()-adjacent surface the
// Ask model picker + conversation-creation guard consume (ADR-0004 D1): a kind
// that registers but does not implement ChatTurnClient is still dispatchable
// for worker executions (Kinds) but is NOT offered for Ask chat. An empty
// Dispatcher yields an empty (non-nil) slice.
func (d *Dispatcher) ChatKinds() []string {
	d.mu.RLock()
	defer d.mu.RUnlock()
	var out []string
	for k, b := range d.bridges {
		if _, ok := b.(ChatTurnClient); ok {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// kindsLocked returns a comma-separated list of registered kinds. Caller
// holds at least RLock.
func (d *Dispatcher) kindsLocked() string {
	kinds := make([]string, 0, len(d.bridges))
	for k := range d.bridges {
		kinds = append(kinds, k)
	}
	if len(kinds) == 0 {
		return "(none)"
	}
	out := ""
	for i, k := range kinds {
		if i > 0 {
			out += ", "
		}
		out += "\"" + k + "\""
	}
	return out
}
