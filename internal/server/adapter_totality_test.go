package server

// adapter_totality_test.go — the SERVER half of the add-a-kind sweep: a dispatcher registration AND
// a READY runtime_adapters seed row, for every kind the builtin catalog declares.
//
// WHY IT IS TWO REQUIREMENTS AND NOT ONE. `dispatcher.Register(kind, bridge)` routes an execution
// that was already CHOSEN; `db.ListReadyAdaptersByKind(kind)` is what decides a task may be chosen
// AT ALL. Registering only the bridge produces `no ready adapters of kind "<k>"` for every worker of
// that kind — a failure this platform has now shipped three times (opencode, orchicon, claude). The
// seed table and the registration line are in different files and were each forgotten separately,
// so a hand-written checklist was never going to hold; the check has to be driven by the catalog.

import (
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/adapter"
)

// kindConstName maps the adapter package's kind constants by NAME, so the source scan can resolve
// `dispatcher.Register(adapter.KindX, ...)`. A future kind whose constant is missing here makes the
// scan report UNRESOLVED rather than silently passing — which is the failure this test must produce
// when a new kind is added to one list and not the others.
var kindConstName = map[string]string{
	"KindOpencode":    adapter.KindOpencode,
	"KindOrchicon":    adapter.KindOrchicon,
	"KindClaude":      adapter.KindClaude,
	"KindAntigravity": adapter.KindAntigravity,
}

// TestAdapterKindTotalityServerRegistration asserts, for every catalog kind, that the server wires
// a dispatcher registration AND a dev capability row.
func TestAdapterKindTotalityServerRegistration(t *testing.T) {
	kinds := catalogKindsForServer(t)
	registered := registeredDispatcherKinds(t, "server.go")
	seeded := map[string]devAdapterSeed{}
	for _, s := range devAdapterSeeds() {
		seeded[s.kind] = s
	}

	for _, kind := range kinds {
		t.Run(kind, func(t *testing.T) {
			if _, ok := registered[kind]; !ok {
				t.Errorf("adapter kind %q is declared by the builtin catalog but has NO dispatcher registration in "+
					"server.go — an unregistered kind is undispatchable. Register a bridge (a placeholder whose Start "+
					"refuses cleanly is better than nothing) and seed its row; those are two separate requirements.", kind)
			}
			seed, ok := seeded[kind]
			if !ok {
				t.Fatalf("adapter kind %q has no dev adapter seed in devAdapterSeeds() (server.go) — register a bridge "+
					"AND seed a runtime_adapters row. Without the row, selectAdapter fails with "+
					"`no ready adapters of kind %q` and every worker on that adapter is undispatchable", kind, kind)
			}
			if seed.caps == nil {
				t.Fatalf("%s (%s) has no capabilities builder — the row would carry an empty payload", seed.id, seed.kind)
			}
			if strings.TrimSpace(seed.caps()) == "" {
				t.Fatalf("%s (%s) produced empty capabilities; selectAdapter cannot judge capability fit", seed.id, seed.kind)
			}
		})
	}

	// Anti-vacuity: the scan must have found the registrations that DO exist. If it parsed nothing,
	// every kind would "fail" — but if it parsed only one form it could silently mis-resolve.
	if len(registered) < 4 {
		t.Fatalf("the server.go scan resolved only %d dispatcher registration(s) (%v) — the parser is broken and "+
			"this guard proves nothing", len(registered), registered)
	}
}

// registeredDispatcherKinds reads the server source and returns the set of kinds passed to
// dispatcher.Register as a first argument, resolving either a string literal or an adapter.Kind*
// constant.
func registeredDispatcherKinds(t *testing.T, path string) map[string]bool {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	out := map[string]bool{}
	const needle = "dispatcher.Register("
	rest := string(body)
	for {
		i := strings.Index(rest, needle)
		if i < 0 {
			break
		}
		rest = rest[i+len(needle):]
		arg := rest
		if j := strings.IndexByte(arg, ','); j >= 0 {
			arg = arg[:j]
		}
		arg = strings.TrimSpace(arg)
		switch {
		case strings.HasPrefix(arg, `"`):
			out[strings.Trim(arg, `"`)] = true
		case strings.HasPrefix(arg, "adapter.Kind"):
			name := strings.TrimPrefix(arg, "adapter.")
			kind, ok := kindConstName[name]
			if !ok {
				// LOUD, not silent: a new kind constant the table does not know about means the author
				// must extend kindConstName. Reporting the raw token keeps the message actionable.
				out[name] = true
				continue
			}
			out[kind] = true
		}
	}
	return out
}

// catalogKindsForServer returns the LIVE builtin catalog as a sorted slice, with an anti-vacuity
// guard.
func catalogKindsForServer(t *testing.T) []string {
	t.Helper()
	set := adapter.BuiltinAdapterKinds()
	if len(set) < 4 {
		t.Fatalf("the builtin catalog declared only %d adapter kind(s) (%v) — the LIVE kind source shrank, so "+
			"every totality assertion in this suite under-covers", len(set), set)
	}
	kinds := make([]string, 0, len(set))
	for k := range set {
		kinds = append(kinds, k)
	}
	sort.Strings(kinds)
	return kinds
}
