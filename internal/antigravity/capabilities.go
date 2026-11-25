package antigravity

import "encoding/json"

// BuildCapabilitiesJSON returns the runtime-adapter capabilities JSON advertised for the
// Antigravity (agy) in-process bridge (adapter kind "antigravity").
//
// WHY THIS EXISTS, and it is not decoration. Dispatch resolves an adapter in TWO places, and the
// bridge registration only satisfies the first:
//
//  1. the Dispatcher — `dispatcher.Register(adapter.KindAntigravity, agyBridge)`, which routes an
//     already-chosen execution to a bridge;
//  2. a READY `runtime_adapters` ROW of the matching kind — `selectAdapter` →
//     `db.ListReadyAdaptersByKind(tenant, kind)`, which is what decides a task may dispatch AT ALL.
//
// Registering only the bridge is the exact black hole that has now been hit THREE times (opencode,
// orchicon, then claude): every worker of that kind fails with `no ready adapters of kind "<k>"`.
// This builder exists so the antigravity kind lands with its row from the start.
//
// THE VALUES ARE DELIBERATELY EMPTY, AND THAT IS THE HONEST ANSWER, NOT AN OMISSION. Child 1 of this
// epic proved only that agy can be launched and that it refuses cleanly when unauthenticated; no
// cancellation, resume, mid-run injection, compaction or transcript telemetry has been demonstrated
// for this kind yet. Advertising a capability the adapter does not have makes `selectAdapter` route
// work that then fails at runtime, which is worse than refusing it up front. The generic keys are
// present (so the control plane's uniform reader sees a well-formed row) and children 5/6 fill them
// as each behaviour lands. The row itself is never empty — see
// internal/server/adapter_seed_test.go, which fails a seed whose builder returns "".
func BuildCapabilitiesJSON() string {
	caps := map[string]any{
		"tools":     []string{},
		"context":   []string{},
		"execution": []string{},
		"telemetry": []string{},
		// NB: these keys are the generic ones the control plane reads uniformly across kinds.
	}
	b, _ := json.Marshal(caps)
	return string(b)
}
