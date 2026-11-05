package db

// pool_sizing_test.go — THE POOL MUST COVER THE PLANE'S OWN PINNED CONNECTIONS.
//
// THE FAILURE THIS PINS, measured on a 6-CPU macOS host: sign-up and local-login hung until the client gave
// up (`db: begin tx: context canceled` in the log) while /healthz returned 200 and the UI loaded. Nobody could
// create an account, so nobody could log in.
//
// The cause was arithmetic. leaderElection (internal/reconciler/manager.go) holds a dedicated *pgxpool.Conn
// per reconciler kind for the process lifetime — pg_try_advisory_lock is session-scoped, so 7 kinds means 7
// connections that never come back. pgx's default ceiling is `max(4, NumCPU)`; with 6 CPUs that is 6, so the
// seven reconcilers took every connection and every request waited forever. A 12-16 CPU workstation leaves
// headroom, which is why the same code was fine on the operator's Windows box and broken on the Mac.

import "testing"

// reconcilerKinds is the count today (server.go registers task, workflow, recovery, scheduled-run, sequence,
// recurring-fire and worktree). Kept as a constant here so the floor assertion below fails loudly if the
// reconciler set grows past what the pool can carry.
const reconcilerKinds = 7

// dispatchConcurrencyDefault mirrors config.Default()'s DispatchConcurrency, each in-flight item holding one
// connection for its short transaction.
const dispatchConcurrencyDefault = 4

// THE REGRESSION: a small host must not size the pool below the plane's own floor.
func TestPoolCeilingCoversPinnedConnectionsOnASmallHost(t *testing.T) {
	// The exact Mac that failed: 6 CPUs, plain DSN, no override.
	got, why := resolvePoolMaxConns("postgres://orchicon:orchicon@localhost:5432/orchicon?sslmode=disable", 6, func(string) string { return "" })
	if why != "default" {
		t.Fatalf("rule = %q, want default", why)
	}
	// What pgx would have used, for the record.
	pgxDefault := 6
	if got <= int32(pgxDefault) {
		t.Fatalf("ceiling = %d on a 6-CPU host; pgx's own default is %d and that is exactly what starved the "+
			"plane — the ceiling must exceed it", got, pgxDefault)
	}
	// And it must clear the pinned floor with room for dispatch and requests.
	floor := int32(reconcilerKinds + dispatchConcurrencyDefault)
	if got < floor+16 {
		t.Fatalf("ceiling = %d, want at least %d (reconcilers %d + dispatch %d + 16 for HTTP and relays) — a "+
			"pool at the pinned floor leaves nothing for a request and every DB-backed endpoint hangs",
			got, floor+16, reconcilerKinds, dispatchConcurrencyDefault)
	}
	if got != defaultPoolMaxConns {
		t.Fatalf("ceiling = %d, want the documented floor %d", got, defaultPoolMaxConns)
	}
}

// A LARGER HOST SCALES UP, so a busy plane is not throttled by the floor.
func TestPoolCeilingScalesWithCPU(t *testing.T) {
	got, _ := resolvePoolMaxConns("postgres://x/y?sslmode=disable", 32, func(string) string { return "" })
	if got != 128 {
		t.Fatalf("ceiling on 32 CPUs = %d, want 128 (NumCPU*4)", got)
	}
}

// AN EXPLICIT CHOICE WINS — in the DSN or in the environment. Silently overriding an operator's stated pool
// size would be the same class of bug in the other direction.
func TestExplicitPoolSizeIsNotOverridden(t *testing.T) {
	// 1. The connection string carries it: leave whatever pgx parsed.
	if got, why := resolvePoolMaxConns("postgres://x/y?sslmode=disable&pool_max_conns=40", 6,
		func(string) string { return "" }); got != 0 {
		t.Fatalf("a DSN carrying pool_max_conns was overridden (ceiling %d, rule %q) — an operator's explicit "+
			"pool size must win", got, why)
	}
	// 2. The environment carries it.
	got, why := resolvePoolMaxConns("postgres://x/y?sslmode=disable", 6, func(k string) string {
		if k == envPoolMaxConns {
			return "50"
		}
		return ""
	})
	if got != 50 || why != "env:"+envPoolMaxConns {
		t.Fatalf("ceiling = %d (rule %q), want 50 from the environment", got, why)
	}
}

// A NONSENSE OVERRIDE IS IGNORED rather than propagated — an empty, negative or unparseable value must fall
// through to the default, never to zero (a pool of 0 would hang the plane exactly as the reported bug did).
func TestGarbageOverrideFallsBackToTheDefault(t *testing.T) {
	for _, v := range []string{"", "0", "-5", "lots", " "} {
		got, why := resolvePoolMaxConns("postgres://x/y?sslmode=disable", 6, func(k string) string {
			if k == envPoolMaxConns {
				return v
			}
			return ""
		})
		if got != defaultPoolMaxConns {
			t.Errorf("override %q gave ceiling %d (rule %q), want the default %d", v, got, why, defaultPoolMaxConns)
		}
	}
}
