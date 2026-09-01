package scheduler

import (
	"fmt"
	"sync"
)

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
}

// NewDispatcher creates an empty Dispatcher. Adapters register via
// Register before the reconciler loop starts.
func NewDispatcher() *Dispatcher {
	return &Dispatcher{bridges: make(map[string]AdapterBridge)}
}

// Register associates the bridge with the given adapter kind (e.g.
// "opencode"). Registering the same kind twice overwrites the previous
// bridge (last registration wins) — it never panics. An empty kind is
// rejected: it could never be resolved (ParseModelRef never yields an
// empty Adapter), so registering it is a programming error surfaced
// loudly at startup.
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
	b, ok := d.bridges[kind]
	if !ok {
		return nil, fmt.Errorf("no adapter bridge registered for kind %q — register it at server construction or fix the worker's model_ref (registered kinds: %s)", kind, d.kindsLocked())
	}
	return b, nil
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
