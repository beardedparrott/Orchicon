package scheduler

import (
	"errors"
	"fmt"
	"sort"
	"sync"
)

// ErrAdapterDisabled is the sentinel Resolve returns for an administratively disabled adapter kind.
var ErrAdapterDisabled = errors.New("adapter disabled")

// ErrAdapterKindUnregistered is the sentinel Resolve returns when no bridge is registered for a
// kind.
var ErrAdapterKindUnregistered = errors.New("no adapter bridge registered")

// Dispatcher routes executions to the AdapterBridge for the adapter kind parsed from the
// execution's model_ref (adapter.ParseModelRef(ref).Adapter).
type Dispatcher struct {
	mu      sync.RWMutex
	bridges map[string]AdapterBridge
	// disabled holds adapter kinds administratively switched OFF for this plane (per-adapter
	// enable/disable, AC 3).
	disabled map[string]struct{}
	// diffLedger is the installed diff-ledger callback pair, applied to every bridge at registration
	// (and to those already registered).
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

// DisabledKinds returns the administratively disabled kinds, sorted.
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

// Register associates the bridge with the given adapter kind (e.g. "opencode").
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
	// A bridge that cannot ledger simply does not implement DiffLedgerSink — the same
	// optional-capability discipline as MessageInjector/Aborter, and never a panic.
	if d.diffLedger != nil {
		if sink, ok := bridge.(DiffLedgerSink); ok {
			sink.SetDiffLedger(d.diffLedger.execution, d.diffLedger.ask)
		}
	}
}

// SetDiffLedger installs the diff-ledger callbacks applied to EVERY bridge from now on, and to
// every bridge already registered.
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

// diffLedgerHooks holds the installed callbacks.
type diffLedgerHooks struct {
	execution DiffLedgerFunc
	ask       DiffLedgerFunc
}

// DiffLedgerCapable reports whether the bridge registered for kind can accept a diff ledger.
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

// Resolve returns the AdapterBridge registered for the given adapter kind.
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

// Kinds returns the registered adapter kinds, sorted, deduped (map keys are unique by
// construction).
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

// ChatKinds returns the registered adapter kinds whose bridge implements the ChatTurnClient (Ask
// chat) capability.
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
