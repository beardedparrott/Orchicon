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
	// private marks an install whose HOST directory must NOT be bind-mounted.
	// The container instead gets a PRIVATE, writable, runtime-uid-owned
	// directory at the same container path, plus — when authFile is set — that
	// one file bound READ-ONLY inside it.
	//
	// opencode's data home is the case this exists for. opencode 2.x writes its
	// log, its repos and its session DB under ~/.local/share/opencode at run
	// time, so a read-only mount breaks the boot outright
	// (EROFS .../opencode/log/opencode.log → `serve handshake: exit status 1`);
	// but mounting the operator's directory read-write would hand every worker
	// write access to the operator's credential AND to a session store it has
	// no business touching. Private + auth-only is the mount that lets the CLI
	// run without either failure.
	private bool
	// authFile is the file inside a PRIVATE install's host directory that is
	// bind-mounted read-only into the container's private copy of it. Empty
	// means no file is shared (the container's copy is entirely its own).
	authFile string
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
				// The data home is PRIVATE (see adapterInstall.private): the CLI must
				// WRITE here, but the operator's credential and session store must
				// not become writable from a worker — so the container gets its own
				// uid-owned directory and only auth.json, read-only, from the host.
				{probe: join(".local", "share", "opencode"), dir: true,
					private: true, authFile: "auth.json"},
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
				// THE TRANSCRIPT HOME IS PRIVATE PER CONTAINER, AND THAT IS A CORRECTNESS FIX.
				//
				// ~/.claude/projects/<encoded-cwd>/<session-id>.jsonl is written at run time, so it must be
				// writable — but it was BIND-MOUNTED from the host, which means every concurrent container
				// wrote into the SAME directory on the operator's machine. Two runs at once interleaved their
				// session state in one tree, and each was free to read the other's transcripts.
				//
				// Private + auth-file is the same shape opencode already uses (see adapterInstall.private for
				// the full reasoning): the container gets its OWN writable copy at the same path, with only
				// the credential bound read-only inside it. The host's tree is never written by a container.
				{probe: join(".claude"), dir: true, private: true,
					// authFile is RELATIVE TO THE PROBE DIR (see adapterPrivateHomes, which joins it onto
					// in.probe) — opencode's "auth.json" is the same shape.
					authFile: ".credentials.json"},
				// ~/.claude.json is the CLI's own config, written on start and on settings changes. It was
				// rw bind-mounted, which is the same shared-write hazard across concurrent runs; private is
				// right for it too. Deliberately NOT an authFile source: the credential lives in
				// .claude/.credentials.json, and sharing two writable config files buys nothing.
				{probe: join(".claude.json"), private: true},
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

// AdapterHostMountRoots returns the host paths a demanded kind's installs are
// mounted FROM — the roots a resolved adapter CLI must sit under to be reachable
// from inside a runtime container.
//
// THE DECLARATION, NOT THE RUNTIME SET: existence is deliberately NOT checked.
// adapterHostMounts does check it, because a `-v` must point at something real;
// this answers a question about the RULE — "would a binary placed here be
// visible to a worker?" — and a caller that stat'd would get a different answer
// depending on whether the operator has installed anything yet.
//
// The consumer is internal/opencode's ContainerVisible, which pairs with the
// binary resolver: 'opencode is installed' is a HOST fact, and on its own it says
// nothing about whether the runtime containers can execute it. Deriving both from
// this one declaration is what keeps the two answers from disagreeing.
func AdapterHostMountRoots(home string, kinds ...string) []string {
	if home == "" {
		return nil
	}
	seen := make(map[string]struct{})
	var out []string
	for _, kind := range kinds {
		installs, declared := adapterInstalls(home, kind)
		if !declared {
			continue
		}
		for _, in := range installs {
			root := in.mount
			if root == "" {
				root = in.probe
			}
			if _, ok := seen[root]; ok {
				continue
			}
			seen[root] = struct{}{}
			out = append(out, root)
		}
	}
	return out
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
	// THE RESOLVED BINARY'S DIRECTORY, MOUNTED AT ITS OWN ABSOLUTE PATH — the mount half of the same fix as
	// adapterBinDirs. Without it the container's PATH names a directory that was never mounted, which is the
	// "installed but the container cannot find it" failure in its container form.
	//
	// A WINDOWS PATH IS REFUSED HERE rather than mounted uselessly: the container is Linux, so a PE
	// executable on its PATH is a directory it can list and not execute. The refusal is reported by
	// CheckAdaptersForContainer, which the daemon calls, so the operator gets one actionable sentence
	// instead of a container that starts and then cannot spawn its adapter.
	for _, kind := range kinds {
		bin := adapter.ResolveBinary(adapter.HostGOOS(), home, kind, os.Getenv)
		if bin == "" {
			continue
		}
		if ok, _ := adapter.ContainerUsable(bin, "linux"); !ok {
			continue
		}
		dir := filepath.Dir(bin)
		// SKIP WHEN THE DECLARED TABLE ALREADY MOUNTS IT. The declared dirs are mounted below, and a second
		// `-v` of the same pair is not harmless noise: it doubles the argv, and the mount tests read that
		// argv as the contract. So this adds a mount only for an install the table does not know about —
		// which is the case it exists for.
		if alreadyDeclaredMounts(bin, home, kind) {
			continue
		}
		if st, err := os.Stat(dir); err == nil && st.IsDir() {
			args = append(args, "-v", dir+":"+dir+":ro")
		}
	}
	for _, kind := range kinds {
		installs, declared := adapterInstalls(home, kind)
		if !declared {
			continue
		}
		for _, in := range installs {
			if in.private {
				// Not a host bind at all: standardHostMountArgs gives the
				// container a private writable copy (adapterPrivateHomes).
				continue
			}
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
			// No `,z` relabel yet — ADR-0013 D3.2 lands it with the ingress
			// slice (architecture note D-4); see the same note at the daemon's
			// -v loop in daemon.go createContainer.
			mode := ":ro"
			if in.rw {
				mode = ":rw"
			}
			args = append(args, "-v", src+":"+src+mode)
		}
	}
	return args
}

// privateHome is one adapter install's PRIVATE, writable container directory:
// the path to give the container (identical inside and out, as every other
// mount is) and, when the operator has one to share, the credential file to
// bind read-only into it.
type privateHome struct {
	dir      string
	authFile string
}

// adapterPrivateHomes returns the private homes a demanded kind contributes,
// in declaration order. Probe-gated like every other install, and for good
// reason: with no host directory to mount from there is nothing to share, and
// the container's own copy is then created by the CLI itself under a $HOME
// that standardHostMountArgs has already made writable by the runtime uid.
func adapterPrivateHomes(home string, kinds ...string) []privateHome {
	if home == "" {
		return nil
	}
	var out []privateHome
	for _, kind := range kinds {
		installs, declared := adapterInstalls(home, kind)
		if !declared {
			continue
		}
		for _, in := range installs {
			if !in.private {
				continue
			}
			st, err := os.Stat(in.probe)
			if err != nil || !st.IsDir() {
				continue
			}
			ph := privateHome{dir: in.probe}
			if in.authFile != "" {
				af := filepath.Join(in.probe, in.authFile)
				if ast, aerr := os.Stat(af); aerr == nil && !ast.IsDir() {
					ph.authFile = af
				}
			}
			out = append(out, ph)
		}
	}
	return out
}

// adapterBinDirs returns the container PATH dirs a demanded kind's mounted
// CLI needs. Derived from the same install table the mounts come from, so a
// mounted launcher is always reachable by name.
func adapterBinDirs(home, kind string) []string {
	// THE RESOLVED BINARY'S DIRECTORY COMES FIRST, and it is the whole point of this change.
	//
	// This used to return ONE hardcoded directory per kind, which the container's PATH was then built from.
	// Meanwhile each adapter's own host resolver had learned about Windows install locations — npm's
	// node_modules, WinGet's package directories — so the plane could RUN an adapter the container could not
	// FIND. Two lists, two answers, and the container is where the disagreement surfaced.
	//
	// internal/adapter owns the candidate list BOTH sides now read (it imports neither adapter package, so
	// there is no cycle), and this asks it where the binary actually is.
	var dirs []string
	if bin := adapter.ResolveBinary(adapter.HostGOOS(), home, kind, os.Getenv); bin != "" {
		dirs = append(dirs, filepath.Dir(bin))
	}
	// THE DECLARED DIRECTORY IS KEPT AS A FALLBACK, for a container whose mounts already carry an adapter
	// installed at the documented location (a baked image, an operator's own Linux install) even though this
	// host has none to resolve.
	switch normalizedKind(kind) {
	case adapter.DefaultAdapterKind:
		dirs = append(dirs, filepath.Join(home, ".opencode", "bin"))
	case adapter.KindClaude:
		dirs = append(dirs, filepath.Join(home, ".local", "bin"))
	}
	return dirs
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
		// THE IMAGE'S ADAPTER WINS, SO ITS DIRECTORY IS NOT SHADOWED.
		//
		// The PATH this returns is PREPENDED to the container's, ahead of /usr/local/bin — which is exactly
		// where an npm-installed adapter lives. Leaving a host bin dir in the prefix for a kind the image
		// supplies would make the container run the OPERATOR'S adapter instead of the image's: the image
		// install would be dead weight, two hosts on the same image could run different adapter versions,
		// and the container would still depend on the host in the way it no longer needs to.
		//
		// THE ESCAPE HATCH IS EXPLICIT. ORCHICON_OPENCODE_BIN / ORCHICON_CLAUDE_BIN naming a path means the
		// operator has CHOSEN a specific build — that is a decision to honour, so the host dir is kept. An
		// ordinary discovered install is not a decision, and the image wins.
		if _, inImage := imageSuppliesAdapter(kind); inImage && strings.TrimSpace(os.Getenv(adapter.BinEnv(kind))) == "" {
			continue
		}
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

// imageAdapterKind is one adapter kind the RUNTIME IMAGE supplies itself.
//
// THE SOURCE OF THIS KNOWLEDGE MATTERS. It is not a guess about what an image happens to contain: the stock
// variants install the adapters in their Dockerfiles (deploy/runtime/Dockerfile.gui and .dev, the npm block
// near the end), and a generated custom image does the same by default (internal/runtimeimage's
// adapterInstallBlock, driven by the include_adapters column). So "the image supplies it" is a property of how
// THIS PRODUCT builds its images, declared here once.
//
// WHY HAVING IT CHANGES ANYTHING. Before, the container's adapter could only come from the HOST, bind-mounted
// in — which is why the daemon refused a Windows host outright (a .exe cannot run in a Linux container) and
// why the host's PATH had to shadow everything. With the adapter in the image, the container is
// self-contained: the host contributes CREDENTIALS, not the binary.
type imageAdapterKind struct {
	// kind is the adapter kind (adapter.DefaultAdapterKind, adapter.KindClaude, ...).
	kind string
	// bin is the executable NAME the image installs on PATH.
	bin string
}

// imageAdapters is the set the shipped images install, keyed by kind.
//
// KEEP IN STEP WITH deploy/runtime/Dockerfile.{gui,dev} AND internal/runtimeimage.adapterInstallBlock. Those
// three are the same promise stated in three formats, and the mount-never-bake guard already pins the other
// direction (no adapter in a PUBLISHED image), so the two lists cannot silently diverge in the way that
// matters most.
var imageAdapters = []imageAdapterKind{
	{kind: adapter.DefaultAdapterKind, bin: "opencode"},
	{kind: adapter.KindClaude, bin: "claude"},
}

// imageSuppliesAdapter reports whether the image for THIS request provides the kind itself.
//
// THE REQUEST'S IMAGE DECIDES, and it can only be known from the row that built it — so the answer is
// CONSERVATIVE by design: a kind is treated as image-supplied unless the request proves otherwise, and the
// one proof available is that the operator demanded a kind the shipped set does not name. Refusing to start a
// container on a guess would recreate the very failure this exists to remove, so the fallback is the
// mount path below, which still works whenever the host can contribute a runnable binary.
func imageSuppliesAdapter(kind string) (imageAdapterKind, bool) {
	for _, a := range imageAdapters {
		if a.kind == kind {
			return a, true
		}
	}
	return imageAdapterKind{}, false
}

// CheckAdaptersForContainer reports, for the run's demanded adapter kinds, any kind that CANNOT be served in
// the container by ANY route — the image, or a mountable host install — and says why.
//
// IT USED TO REPORT "the host's adapter is a Windows executable", and that refusal was correct while the host
// mount was the ONLY source: the container would start and the first session spawn would fail on an exec
// format nothing pointed at. It became a hard stop for every container create once the image started shipping
// the adapter, because the host's binary format stopped being the question.
//
// THE RULE NOW, in order:
//
//  1. THE IMAGE SUPPLIES IT -> fine, whatever the host has. The host may still contribute credentials; its
//     binary being a PE is irrelevant, and on Windows that is the difference between container mode working
//     and not working at all.
//  2. NOT IN THE IMAGE, but the host has a runnable (Linux) binary -> fine, it is bind-mounted as before.
//  3. NEITHER -> a real problem, reported per kind with the two remedies.
//
// A kind with NO host adapter at all is still not an error on its own: an absent adapter may simply be unused
// by a workflow whose steps do not resolve to it, and "not installed" has a different remedy from "the wrong
// kind of binary".
func CheckAdaptersForContainerWith(home string, kinds ...string) []string {
	if strings.TrimSpace(home) == "" {
		// NO HOME CONFIGURED MEANS NO ADAPTER IS MOUNTED, so there is nothing to judge.
		return nil
	}
	return containerAdapterProblems(func(kind string) (string, error) {
		return adapter.ResolveBinary(adapter.HostGOOS(), home, kind, os.Getenv), nil
	}, imageSuppliesAdapter, kinds...)
}

// containerAdapterProblems is the DECISION, with its two inputs injected so both arms are reachable in a test.
//
// The shipped inputs make the refusal unreachable — every kind the product ships is image-supplied — so a
// guard that could only be exercised through them would stop refusing anything the day an adapter left the
// image, silently. resolveBin returning an error is treated as "cannot judge", which is not a refusal.
func containerAdapterProblems(resolveBin func(kind string) (string, error),
	imageSupplies func(kind string) (imageAdapterKind, bool), kinds ...string) []string {
	var problems []string
	for _, kind := range kinds {
		if _, ok := imageSupplies(kind); ok {
			continue // (1) the image has it
		}
		bin, err := resolveBin(kind)
		if err != nil || bin == "" {
			continue // absent or unjudgeable, and either may be intentional — see the note above
		}
		if ok, why := adapter.ContainerUsable(bin, "linux"); !ok {
			problems = append(problems, kind+": "+why)
		}
	}
	return problems
}
// CheckAdaptersForContainer reports, for the run's demanded adapter kinds, any adapter that CANNOT execute
// inside a runtime container — and says why for each.
//
// THIS IS THE ONE PLACE THE OPERATOR HEARS ABOUT IT. A container created with an unrunnable adapter does not
// fail at create: it starts, the supervisor runs, and the FIRST session spawn dies with whatever the kernel
// says about a PE file — inside the container, minutes later, with nothing pointing back at the mount. The
// daemon calls this before it creates the container so the failure lands where the operator can act on it.
//
// AN EMPTY RESULT MEANS EVERYTHING IS FINE. A kind with NO adapter resolved at all is reported too: "not
// installed" and "installed but the wrong binary" have different remedies (install it, versus install the
// LINUX build), and conflating them is how the operator ends up installing something twice.
func CheckAdaptersForContainer(home string, kinds ...string) []string {
	// DELEGATES to the image-aware check. Kept as a name because callers and tests reference it, but the
	// judgement behind it is now "can ANY route serve this kind", not "is the host's binary Linux".
	return CheckAdaptersForContainerWith(home, kinds...)
}

// alreadyDeclaredMounts reports whether the install table already makes this BINARY reachable, so the
// resolved directory does not produce a duplicate `-v`.
//
// IT MATCHES THE BINARY, NOT ONLY THE DIRECTORY, and that distinction is the claude case: its table mounts
// the LAUNCHER FILE (~/.local/bin/claude, a symlink into the install root) rather than its directory, so a
// directory-only comparison adds a second mount of the same thing and doubles the argv. Either form means the
// container can already reach the binary, which is all this asks.
func alreadyDeclaredMounts(bin, home, kind string) bool {
	installs, ok := adapterInstalls(home, kind)
	if !ok {
		return false
	}
	cleanBin := filepath.Clean(bin)
	cleanDir := filepath.Dir(cleanBin)
	for _, in := range installs {
		src := in.mount
		if src == "" {
			src = in.probe
		}
		if src == "" || in.private {
			continue
		}
		cleanSrc := filepath.Clean(src)
		// The declared source is the binary itself, the directory holding it, OR an ANCESTOR of it. The
		// third case is the opencode table: it mounts the ~/.opencode ROOT, so a binary resolved at
		// ~/.opencode/bin/opencode is already inside a mounted tree and a second mount adds nothing.
		if cleanSrc == cleanBin || cleanSrc == cleanDir {
			return true
		}
		if rel, err := filepath.Rel(cleanSrc, cleanBin); err == nil && !strings.HasPrefix(rel, "..") {
			return true
		}
	}
	return false
}
