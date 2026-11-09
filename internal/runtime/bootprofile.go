package runtime

import (
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/beardedparrott/orchicon/internal/adapter"
)

// Per-run container BOOT PROFILE.

// nativeAdapterKind is the dispatcher-registered native bridge kind (ADR-0004): its sessions
// execute INSIDE the run's container through the supervisor's one-shot exec path.
const nativeAdapterKind = "orchicon"

// normalizedKind maps an empty/whitespace kind to the default adapter kind (opencode), mirroring
// adapter.AdapterDemandSet's conservative rule for an empty/unresolvable ref.
func normalizedKind(kind string) string {
	if k := strings.TrimSpace(kind); k != "" {
		return k
	}
	return adapter.DefaultAdapterKind
}

// serveDependentKind reports whether an adapter kind needs an IN-CONTAINER serve.
func serveDependentKind(kind string) bool {
	return (&Lifecycle{}).ServeDependent(kind)
}

// requestedKinds returns the effective boot profile for a create request, sorted and deduped.
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

// serveKindsFor filters a boot profile down to the kinds whose dispatch path needs an in-container
// serve (today: opencode only).
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
	// dir requires probe to be a directory; otherwise probe must exist and NOT be a directory.
	dir bool
	// rw mounts this install READ-WRITE.
	rw bool
	// The container instead gets a PRIVATE, writable, runtime-uid-owned directory at the same container
	// path, plus — when authFile is set — that one file bound READ-ONLY inside it.
	private bool
	// authFile is the file inside a PRIVATE install's host directory that is bind-mounted into the
	// container's private copy of it.
	authFile string
	// authWritable mounts authFile READ-WRITE instead of read-only.
	authWritable bool
	// fingerprint lists the sub-trees of this install whose change must invalidate a WARM pooled
	// container.
	fingerprint []string
}

// consumptionWitness is one file+needle PROVING a consumption axis is actually wired.
type consumptionWitness struct {
	// file is a path relative to this package dir (internal/runtime).
	file string
	// needle is a literal substring that MUST appear in file.
	needle string
}

// The negative-axis notes.
const (
	// hostServeUnionLimitationNote states opencode's HOST-SERVE limitation precisely: a positive
	// runUnionAtServe covers the CONTAINER serve (baked once at container creation).
	hostServeUnionLimitationNote = "opencode consumes the per-session bundle (project-owned ∪ own) via hostResolvedSet and a CONTAINER serve receives the run-level union once at creation (resolveRunUnion → RunMCP). HOST-SERVE LIMITATION: an IN-PROCESS host serve builds its OPENCODE_CONFIG_CONTENT ONCE per serve process (internal/opencode/servehost.go serveConfig, BuildConfigContent) and that process lives for the plane, so unioning several projects' MCP sets onto it would make one project's servers visible in another project's session — a leak with no consent boundary. A session whose set differs therefore runs on a serve built for that set (HostServePool, internal/opencode/servepool.go); the host serve resolves exactly ONE set, the PROJECT scope, and never a cross-project union. The run-level (per-container) union is delivered once, at container creation, and is NOT what a host serve carries."

	// claudeNoServeNote: claude has no in-container HTTP serve at all.
	claudeNoServeNote = "claude is a streaming-stdio adapter with NO in-container HTTP serve (internal/claude/adapter.go: servePortFor(\"claude\") == 0 — its dispatch path is a host CLI process streaming stdio over the daemon's duplex transport), so there is no once-per-run serve to bake the run-level union into. It receives the per-SESSION worker scope (the project's owned definitions ∪ THE EXECUTING VERSION's inline specs) at each launch via ScopeRef{Kind: ScopeWorker} → resolveMCP (internal/claude/mcpresolve.go), which this adapter renders. A per-container run-level union is unavailable by construction, not by omission."

	// nativeNoServeNote: the native bridge execs one-shot, no serve.
	nativeNoServeNote = "the native (orchicon) bridge execs sessions ONE-SHOT inside the run container through the supervisor's exec path with no opencode serve (bootprofile.go nativeAdapterKind), so there is no once-per-run serve to receive the run-level union. The native bridge receives the per-session worker scope (project-owned ∪ own) via ResolveExecutionMCP (internal/orchicon/mcptools.go), which it starts its MCP manager from."
)

// adapterClass is the ONE classification of an adapter kind.
type adapterClass struct {
	installs []adapterInstall
	// perSessionBundle: the kind resolves AND consumes the per-session bundle (project-owned ∪ the
	// scope's own specs).
	perSessionBundle    bool
	perSessionWitnesses []consumptionWitness
	// REQUIRED true for every serve-dependent kind (ServeDependent==true) — the gate fails a
	// serve-dependent kind that declares it false.
	runUnionAtServe   bool
	runUnionWitnesses []consumptionWitness
	// note: the explicit declaration for any NEGATIVE axis, naming what the kind does NOT do and why.
	note string
}

// classifyAdapter is the ONE adapter-kind classification the daemon mounts from AND both guards
// read.
func classifyAdapter(home, kind string) (adapterClass, bool) {
	join := func(parts ...string) string {
		return filepath.Join(append([]string{home}, parts...)...)
	}
	switch normalizedKind(kind) {
	case adapter.DefaultAdapterKind: // opencode
		return adapterClass{
			installs: []adapterInstall{
				{probe: join(".config", "opencode"), dir: true},
				// The data home is PRIVATE (see adapterInstall.private): the CLI must WRITE here, but the
				// operator's credential and session store must not become writable from a worker.
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
				// The CONTAINER-serve path — the axis this declares.
				{file: "lifecycle.go", needle: "resolveRunUnion"},
				{file: "../opencode/adapter.go", needle: "union.Servers"},
			},
			note: hostServeUnionLimitationNote,
		}, true
	case adapter.KindClaude:
		return adapterClass{
			installs: []adapterInstall{
				// THE TRANSCRIPT HOME IS PRIVATE PER CONTAINER, AND THAT IS A CORRECTNESS FIX.
				{probe: join(".claude"), dir: true, private: true,
					// authFile is RELATIVE TO THE PROBE DIR (see adapterPrivateHomes, which joins it onto in.probe) —
					// opencode's "auth.json" is the same shape.
					authFile: ".credentials.json", authWritable: true},
				// ~/.claude.json is the CLI's own config, written on start and on settings changes.
				{probe: join(".claude.json"), private: true},
				// NEVER probe a version string (versions/2.1.261): its basename survives the bake guard's <3-char
				// filter and becomes a forbidden-bake needle.
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
		// The orchicon binary is the PRODUCT binary: the daemon bind-mounts it at /usr/local/bin/orchicon
		// (never an adapter CLI, never a host-home install).
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

// adapterInstalls returns the host installs a kind contributes to a demanding container, and
// whether the kind is CLASSIFIED (declared).
func adapterInstalls(home, kind string) ([]adapterInstall, bool) {
	c, ok := classifyAdapter(home, kind)
	return c.installs, ok
}

// adapterInstallPaths returns the host install paths a kind's adapter CLI occupies, plus whether
// the kind is classified.
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

// AdapterHostMountRoots returns the host paths a demanded kind's installs are mounted FROM — the
// roots a resolved adapter CLI must sit under to be reachable from inside a runtime container.
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

// adapterHostMounts returns the daemon -v args for the adapter installs a demanded kind
// contributes.
func adapterHostMounts(home string, kinds ...string) []string {
	if home == "" {
		return nil
	}
	var args []string
	// THE RESOLVED BINARY'S DIRECTORY, MOUNTED AT ITS OWN ABSOLUTE PATH — the mount half of the same
	// fix as adapterBinDirs.
	for _, kind := range kinds {
		bin := adapter.ResolveBinary(adapter.HostGOOS(), home, kind, os.Getenv)
		if bin == "" {
			continue
		}
		if ok, _ := adapter.ContainerUsable(bin, "linux"); !ok {
			continue
		}
		dir := filepath.Dir(bin)
		// SKIP WHEN THE DECLARED TABLE ALREADY MOUNTS IT.
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
			// No `,z` relabel yet — ADR-0013 D3.2 lands it with the ingress slice (architecture note D-4); see
			// the same note at the daemon's -v loop in daemon.go createContainer.
			mode := ":ro"
			if in.rw {
				mode = ":rw"
			}
			args = append(args, "-v", src+":"+src+mode)
		}
	}
	return args
}

// privateHome is one adapter install's PRIVATE, writable container directory: the path to give the
// container (identical inside and out, as every other mount is).
type privateHome struct {
	dir      string
	authFile string
	// authWritable propagates adapterInstall.authWritable to the argv builder.
	authWritable bool
	// isFile marks a private install that is a REGULAR FILE rather than a directory (~/.claude.json).
	isFile bool
}

// adapterPrivateHomes returns the private homes a demanded kind contributes, in declaration order.
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
			if err != nil {
				continue
			}
			// A PRIVATE FILE IS DECLARED AND MUST BE MOUNTED. Skipping it was the silent drop described on
			// privateHome.isFile.
			if !st.IsDir() {
				out = append(out, privateHome{dir: in.probe, isFile: true})
				continue
			}
			ph := privateHome{dir: in.probe, authWritable: in.authWritable}
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

// adapterBinDirs returns the container PATH dirs a demanded kind's mounted CLI needs.
func adapterBinDirs(home, kind string) []string {
	// THE RESOLVED BINARY'S DIRECTORY COMES FIRST, and it is the whole point of this change.
	var dirs []string
	if bin := adapter.ResolveBinary(adapter.HostGOOS(), home, kind, os.Getenv); bin != "" {
		dirs = append(dirs, filepath.Dir(bin))
	}
	// THE DECLARED DIRECTORY IS KEPT AS A FALLBACK, for a container whose mounts already carry an
	// adapter installed at the documented location (a baked image.
	switch normalizedKind(kind) {
	case adapter.DefaultAdapterKind:
		dirs = append(dirs, filepath.Join(home, ".opencode", "bin"))
	case adapter.KindClaude:
		dirs = append(dirs, filepath.Join(home, ".local", "bin"))
	}
	return dirs
}

// adapterPathPrefix returns the EXISTING bin dirs of the demanded kinds, sorted and deduped.
func adapterPathPrefix(home string, kinds ...string) []string {
	return adapterPathPrefixFor(home, adapter.HostGOOS(), kinds...)
}

// adapterPathPrefixFor is adapterPathPrefix with the host platform injected, so the non-Linux arm is
// reachable from a Linux gate.
func adapterPathPrefixFor(home, hostGOOS string, kinds ...string) []string {
	if home == "" {
		return nil
	}
	seen := make(map[string]struct{})
	var out []string
	for _, kind := range kinds {
		switch normalizedKind(kind) {
		case adapter.KindClaude, adapter.DefaultAdapterKind:
			// THE HOST'S CLI IS WHAT THE CONTAINER RUNS, so its bin dir must be on the PATH that precedes
			// /usr/local/bin — where an image-installed CLI lives.
			if ok, _ := adapter.PreferHostAdapterInContainer(hostGOOS, adapter.ResolveBinary(hostGOOS, home, kind, os.Getenv)); !ok {
				continue
			}
		default:
			if _, inImage := imageSuppliesAdapter(kind); inImage && strings.TrimSpace(os.Getenv(adapter.BinEnv(kind))) == "" {
				continue
			}
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
type imageAdapterKind struct {
	// kind is the adapter kind (adapter.DefaultAdapterKind, adapter.KindClaude, ...).
	kind string
	// bin is the executable NAME the image installs on PATH.
	bin string
}

// imageAdapters is the set the shipped images install, keyed by kind.
var imageAdapters = []imageAdapterKind{
	{kind: adapter.DefaultAdapterKind, bin: "opencode"},
	{kind: adapter.KindClaude, bin: "claude"},
}

// imageSuppliesAdapter reports whether the image for THIS request provides the kind itself.
func imageSuppliesAdapter(kind string) (imageAdapterKind, bool) {
	for _, a := range imageAdapters {
		if a.kind == kind {
			return a, true
		}
	}
	return imageAdapterKind{}, false
}

// IT USED TO REPORT "the host's adapter is a Windows executable".
func CheckAdaptersForContainerWith(home string, kinds ...string) []string {
	if strings.TrimSpace(home) == "" {
		// NO HOME CONFIGURED MEANS NO ADAPTER IS MOUNTED, so there is nothing to judge.
		return nil
	}
	return containerAdapterProblems(func(kind string) (string, error) {
		return adapter.ResolveBinary(adapter.HostGOOS(), home, kind, os.Getenv), nil
	}, imageSuppliesAdapter, kinds...)
}

// containerAdapterProblems is the DECISION, with its two inputs injected so both arms are reachable
// in a test.
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
// CheckAdaptersForContainer reports, for the run's demanded adapter kinds, any adapter that CANNOT
// execute inside a runtime container — and says why for each.
func CheckAdaptersForContainer(home string, kinds ...string) []string {
	// DELEGATES to the image-aware check. Kept as a name because callers and tests reference it, but the
	// judgement behind it is now "can ANY route serve this kind", not "is the host's binary Linux".
	return CheckAdaptersForContainerWith(home, kinds...)
}

// IT MATCHES THE BINARY, NOT ONLY THE DIRECTORY, and that distinction is the claude case: its table
// mounts the LAUNCHER FILE (~/.local/bin/claude.
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
		// The declared source is the binary itself, the directory holding it, OR an ANCESTOR of it.
		if cleanSrc == cleanBin || cleanSrc == cleanDir {
			return true
		}
		if rel, err := filepath.Rel(cleanSrc, cleanBin); err == nil && !strings.HasPrefix(rel, "..") {
			return true
		}
	}
	return false
}
