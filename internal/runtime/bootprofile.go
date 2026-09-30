package runtime

import (
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/beardedparrott/orchicon/internal/adapter"
)

// Per-run container BOOT PROFILE.
//
// A runtime container's boot profile is the set of adapter kinds its step
// workers resolve to (adapter.AdapterKind over each step worker's
// model_ref, sorted + deduped). It rides the create request
// (CreateRequest.AdapterKinds) from the plane to the daemon, which derives
// from it:
//
//   - which adapter installs the container gets mounted (the conditional
//     ~/.opencode config/auth/CLI mounts — a native-only run mounts none of
//     them), and
//   - which in-container serves it warms at create time (today exactly one:
//     the opencode serve).
//
// The profile is the RUN-LEVEL half of the ONE shared demand-set primitive
// (adapter.DemandSet / adapter.AdapterDemandSet, Task C): the run-start gate
// and the host serve accumulate the same per-worker model refs into the same
// primitive, so the run's boot profile and the host serve lifecycle can
// never drift (AC 7 — no forked second computation).

// nativeAdapterKind is the dispatcher-registered native bridge kind
// (ADR-0004): its sessions execute INSIDE the run's container through the
// supervisor's one-shot exec path. It needs no in-container serve and no
// adapter-CLI install mount.
const nativeAdapterKind = "orchicon"

// normalizedKind maps an empty/whitespace kind to the default adapter kind
// (opencode), mirroring adapter.AdapterDemandSet's conservative rule for an
// empty/unresolvable ref.
func normalizedKind(kind string) string {
	if k := strings.TrimSpace(kind); k != "" {
		return k
	}
	return adapter.DefaultAdapterKind
}

// serveDependentKind reports whether an adapter kind needs an IN-CONTAINER
// serve. It delegates to Lifecycle.ServeDependent — THE serve-dependency
// predicate the scheduler's run-start gate hands to
// adapter.DemandSet.NeedsServe — called on a zero Lifecycle (the method
// reads only its argument), so this is the same evaluation and never a
// forked second copy of the rule.
func serveDependentKind(kind string) bool {
	return (&Lifecycle{}).ServeDependent(kind)
}

// requestedKinds returns the effective boot profile for a create request,
// sorted and deduped.
//
// A NIL profile is the legacy/unspecified case — a pre-change plane (which
// does not know the field) or a plane that could not resolve a demand at
// all. It means opencode is demanded, i.e. today's unconditional adapter
// mounts + serve: a mixed-version rollout must never silently strip model
// auth from an opencode run. A NON-NIL profile is honored verbatim, so an
// explicitly empty profile ("this run demands no adapter kind") mounts
// nothing.
func requestedKinds(req CreateRequest) []string {
	if req.AdapterKinds == nil {
		return []string{adapter.DefaultAdapterKind}
	}
	seen := make(map[string]struct{}, len(req.AdapterKinds))
	out := make([]string, 0, len(req.AdapterKinds))
	for _, k := range req.AdapterKinds {
		k = normalizedKind(k)
		if _, ok := seen[k]; ok {
			continue
		}
		seen[k] = struct{}{}
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// demandsKind reports whether the request's boot profile demands kind.
func demandsKind(req CreateRequest, kind string) bool {
	kind = normalizedKind(kind)
	for _, k := range requestedKinds(req) {
		if k == kind {
			return true
		}
	}
	return false
}

// serveKindsFor filters a boot profile down to the kinds whose dispatch
// path needs an in-container serve (today: opencode only). The supervisor
// multiplexes one serve per returned kind (AC 2) while native steps keep
// executing through the one-shot exec path.
func serveKindsFor(kinds []string) []string {
	out := make([]string, 0, len(kinds))
	for _, k := range kinds {
		if serveDependentKind(k) {
			out = append(out, normalizedKind(k))
		}
	}
	return out
}

// adapterInstall is one host adapter install the daemon mounts into a
// container when the run's boot profile demands the kind.
type adapterInstall struct {
	// probe is the host path whose existence gates the mount.
	probe string
	// mount is the host path bind-mounted (the container path is the same).
	// Empty means the probe itself is mounted.
	mount string
	// dir requires probe to be a directory; otherwise probe must exist and
	// NOT be a directory. This mirrors the daemon's original os.Stat gate
	// for the opencode install byte for byte.
	dir bool
	// rw mounts this install READ-WRITE. Only claude's config/transcript
	// home (~/.claude + ~/.claude.json) is rw: the CLI writes its JSONL
	// transcript into ~/.claude/projects/<encoded-cwd>/ at run time, so a
	// read-only mount would break the session outright.
	rw bool
	// fingerprint lists the sub-trees of this install whose change must
	// invalidate a WARM pooled container. Empty = not fingerprinted
	// (config/transcript homes change every session and must never churn
	// the pool). Derived by adapterFingerprintRoots (hostfp.go).
	fingerprint []string
}

// consumptionWitness is one file+needle PROVING a consumption axis is
// actually wired. It is a literal substring of an existing source file, so a
// declaration that is never backed by real wiring FAILS the conformance gate
// (internal/runtime/adapter_bundle_conformance_test.go). The needle is
// checked by READING the file, never by importing the package — the runtime
// package cannot import internal/opencode|claude|orchicon (no such import
// exists in any internal/runtime file), so a source scan is the only
// mechanism available across that boundary. Same technique as
// internal/neverallow/share_test.go.
type consumptionWitness struct {
	// file is a path relative to this package dir (internal/runtime).
	file string
	// needle is a literal substring that MUST appear in file.
	needle string
}

// The negative-axis notes. Each NAMES what the kind does NOT do and why — the
// gate requires a non-empty note for every axis a kind leaves negative, so
// an unexplained negative is a failure, not a silence.
const (
	// hostServeUnionLimitationNote states opencode's HOST-SERVE limitation
	// precisely: a positive runUnionAtServe covers the CONTAINER serve (baked
	// once at container creation), but the in-process HOST serve is a
	// different artifact with a different constraint.
	hostServeUnionLimitationNote = "opencode consumes the per-session bundle (project-owned ∪ own) via hostResolvedSet and a CONTAINER serve receives the run-level union once at creation (resolveRunUnion → RunMCP). HOST-SERVE LIMITATION: an IN-PROCESS host serve builds its OPENCODE_CONFIG_CONTENT ONCE per serve process (internal/opencode/servehost.go serveConfig, BuildConfigContent) and that process lives for the plane, so unioning several projects' MCP sets onto it would make one project's servers visible in another project's session — a leak with no consent boundary. A session whose set differs therefore runs on a serve built for that set (HostServePool, internal/opencode/servepool.go); the host serve resolves exactly ONE set, the PROJECT scope, and never a cross-project union. The run-level (per-container) union is delivered once, at container creation, and is NOT what a host serve carries."

	// claudeNoServeNote: claude has no in-container HTTP serve at all.
	claudeNoServeNote = "claude is a streaming-stdio adapter with NO in-container HTTP serve (internal/claude/adapter.go: servePortFor(\"claude\") == 0 — its dispatch path is a host CLI process streaming stdio over the daemon's duplex transport), so there is no once-per-run serve to bake the run-level union into. It receives the per-SESSION worker scope (the project's owned definitions ∪ THE EXECUTING VERSION's inline specs) at each launch via ScopeRef{Kind: ScopeWorker} → resolveMCP (internal/claude/mcpresolve.go), which this adapter renders. A per-container run-level union is unavailable by construction, not by omission."

	// nativeNoServeNote: the native bridge execs one-shot, no serve.
	nativeNoServeNote = "the native (orchicon) bridge execs sessions ONE-SHOT inside the run container through the supervisor's exec path with no opencode serve (bootprofile.go nativeAdapterKind), so there is no once-per-run serve to receive the run-level union. The native bridge receives the per-session worker scope (project-owned ∪ own) via ResolveExecutionMCP (internal/orchicon/mcptools.go), which it starts its MCP manager from."
)

// adapterClass is the ONE classification of an adapter kind. It carries the
// mount half (installs — read by the daemon's mounts and by
// adapter_bake_guard_test.go) PLUS the two bundle-consumption axes read by
// adapter_bundle_conformance_test.go, so the mount guard and the conformance
// gate read ONE declaration and cannot drift.
type adapterClass struct {
	installs []adapterInstall
	// perSessionBundle: the kind resolves AND consumes the per-session bundle
	// (project-owned ∪ the scope's own specs). Positive ⇒ perSessionWitnesses
	// required; negative ⇒ note must explain.
	perSessionBundle    bool
	perSessionWitnesses []consumptionWitness
	// runUnionAtServe: the kind's container-serve path receives the RUN-LEVEL
	// union at container creation. REQUIRED true for every serve-dependent
	// kind (ServeDependent==true) — the gate fails a serve-dependent kind that
	// declares it false.
	runUnionAtServe   bool
	runUnionWitnesses []consumptionWitness
	// note: the explicit declaration for any NEGATIVE axis, naming what the
	// kind does NOT do and why. Also carries opencode's HOST-SERVE limitation.
	// The gate requires it to be non-empty whenever an axis is negative.
	note string
}

// classifyAdapter is the ONE adapter-kind classification the daemon mounts
// from AND both guards read. adapterInstalls is a thin wrapper over it (the
// mount half), so existing callers and tests compile unchanged and the two
// concerns cannot diverge.
//
// declared == false is a guard failure, never a silent pass: a kind the
// builtin catalog declares but this switch has not classified (the next
// adapter — claude/codex-shaped growth) must fail the mount-never-bake guard
// AND the bundle-conformance gate loudly instead of quietly doing nothing.
func classifyAdapter(home, kind string) (adapterClass, bool) {
	join := func(parts ...string) string {
		return filepath.Join(append([]string{home}, parts...)...)
	}
	switch normalizedKind(kind) {
	case adapter.DefaultAdapterKind: // opencode
		return adapterClass{
			installs: []adapterInstall{
				{probe: join(".config", "opencode"), dir: true},
				{probe: join(".local", "share", "opencode"), dir: true},
				{probe: join(".opencode", "bin", "opencode"), mount: join(".opencode"),
					fingerprint: []string{join(".opencode", "bin"), join(".opencode", "node_modules")}},
			},
			perSessionBundle: true,
			perSessionWitnesses: []consumptionWitness{
				{file: "../opencode/adapter.go", needle: "hostResolvedSet"},
			},
			runUnionAtServe: true,
			runUnionWitnesses: []consumptionWitness{
				// The CONTAINER-serve path — the axis this declares. resolveRunUnion
				// (lifecycle.go) is the resolver; adapter.RuntimeServeConfig receives
				// its `union` and bakes `RunMCP: union.Servers` into the container's
				// OPENCODE_CONFIG_CONTENT ONCE at creation. NOTE: this is NOT
				// servehost.go's `RunMCP` — that is the in-process HOST serve, which
				// resolves exactly ONE set (the PROJECT scope) and never the run
				// union (see hostServeUnionLimitationNote); witnessing it here would
				// assert the wrong artifact and false-assure the axis.
				{file: "lifecycle.go", needle: "resolveRunUnion"},
				{file: "../opencode/adapter.go", needle: "union.Servers"},
			},
			note: hostServeUnionLimitationNote,
		}, true
	case adapter.KindClaude:
		return adapterClass{
			installs: []adapterInstall{
				// The config/transcript home: the CLI writes
				// ~/.claude/projects/<encoded-cwd>/<session-id>.jsonl at run time,
				// so these two MUST be read-write. NOT fingerprinted (they change
				// every session and would churn the warm pool).
				{probe: join(".claude"), dir: true, rw: true},
				{probe: join(".claude.json"), rw: true},
				// The launcher is a SYMLINK into the install root below, mounted
				// at its IDENTICAL absolute host path so the link resolves. Mount
				// it WITHOUT the install root and the link dangles in-container.
				// NEVER probe a version string (versions/2.1.261): its basename
				// survives the bake guard's <3-char filter and becomes a
				// forbidden-bake needle. Probing .local/bin/claude and
				// .local/share/claude yields only the benign needles claude and
				// claude — never a version.
				{probe: join(".local", "bin", "claude"),
					fingerprint: []string{join(".local", "bin", "claude")}},
				{probe: join(".local", "share", "claude"), dir: true,
					fingerprint: []string{join(".local", "share", "claude")}},
			},
			perSessionBundle: true,
			perSessionWitnesses: []consumptionWitness{
				{file: "../claude/session.go", needle: "ScopeWorker"},
			},
			runUnionAtServe: false,
			note:            claudeNoServeNote,
		}, true
	case nativeAdapterKind:
		// The orchicon binary is the PRODUCT binary: the daemon bind-mounts
		// it at /usr/local/bin/orchicon (never an adapter CLI, never a
		// host-home install). It contributes no adapter mount, but it IS
		// classified — the mount-never-bake rule for it is unchanged.
		return adapterClass{
			perSessionBundle: true,
			perSessionWitnesses: []consumptionWitness{
				{file: "../orchicon/mcptools.go", needle: "ResolveExecutionMCP"},
			},
			runUnionAtServe: false,
			note:            nativeNoServeNote,
		}, true
	}
	return adapterClass{}, false
}

// adapterInstalls returns the host installs a kind contributes to a demanding
// container, and whether the kind is CLASSIFIED (declared). Each install
// carries its own read-only/read-write mode (see adapterInstall.rw).
//
// It is a THIN WRAPPER over classifyAdapter (the ONE declaration): the mount
// half lives there, so the daemon's mounts and BOTH guards
// (adapter_bake_guard_test.go and adapter_bundle_conformance_test.go) read a
// single classification and cannot drift.
func adapterInstalls(home, kind string) ([]adapterInstall, bool) {
	c, ok := classifyAdapter(home, kind)
	return c.installs, ok
}

// adapterInstallPaths returns the host install paths a kind's adapter CLI
// occupies, plus whether the kind is classified. It is the single source of
// truth shared by the daemon's mounts (adapterInstalls) and the
// mount-never-bake guard, so the mounted set and the forbidden-bake set can
// never drift.
func adapterInstallPaths(home, kind string) ([]string, bool) {
	installs, declared := adapterInstalls(home, kind)
	if !declared {
		return nil, false
	}
	out := make([]string, 0, len(installs))
	for _, in := range installs {
		out = append(out, in.probe)
	}
	return out, true
}

// adapterHostMounts returns the daemon -v args for the adapter installs a
// demanded kind contributes, emitting only installs whose probe exists with
// the required file/dir shape (mirroring the original inline os.Stat gate).
// home == "" (no host home configured) yields nothing.
func adapterHostMounts(home string, kinds ...string) []string {
	if home == "" {
		return nil
	}
	var args []string
	for _, kind := range kinds {
		installs, declared := adapterInstalls(home, kind)
		if !declared {
			continue
		}
		for _, in := range installs {
			src := in.mount
			if src == "" {
				src = in.probe
			}
			st, err := os.Stat(in.probe)
			if err != nil {
				continue
			}
			if in.dir && !st.IsDir() {
				continue
			}
			if !in.dir && st.IsDir() {
				continue
			}
			mode := ":ro"
			if in.rw {
				mode = ":rw"
			}
			args = append(args, "-v", src+":"+src+mode)
		}
	}
	return args
}

// adapterBinDirs returns the container PATH dirs a demanded kind's mounted
// CLI needs. Derived from the same install table the mounts come from, so a
// mounted launcher is always reachable by name.
func adapterBinDirs(home, kind string) []string {
	switch normalizedKind(kind) {
	case adapter.DefaultAdapterKind:
		return []string{filepath.Join(home, ".opencode", "bin")}
	case adapter.KindClaude:
		return []string{filepath.Join(home, ".local", "bin")}
	}
	return nil
}

// adapterPathPrefix returns the EXISTING bin dirs of the demanded kinds,
// sorted and deduped. Stat-filtered so a prefix never points at an absent
// host dir (the invariant daemon.go's standardHostMountArgs doc states).
// Empty kinds yield nil.
func adapterPathPrefix(home string, kinds ...string) []string {
	if home == "" {
		return nil
	}
	seen := make(map[string]struct{})
	var out []string
	for _, kind := range kinds {
		for _, dir := range adapterBinDirs(home, kind) {
			if _, ok := seen[dir]; ok {
				continue
			}
			st, err := os.Stat(dir)
			if err != nil || !st.IsDir() {
				continue
			}
			seen[dir] = struct{}{}
			out = append(out, dir)
		}
	}
	sort.Strings(out)
	return out
}
