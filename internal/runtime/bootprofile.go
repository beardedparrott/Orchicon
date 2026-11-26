package runtime

import (
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/beardedparrott/orchicon/internal/adapter"
)

// Per-run container BOOT PROFILE.

// nativeAdapterKind is the dispatcher-registered native bridge kind: its sessions
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

	// antigravityNoServeNote: agy has no in-container serve and the bridge is a stub.
	antigravityNoServeNote = "antigravity (agy) has NO in-container HTTP serve — its CLI is a stdio process and nothing about this kind dials a port, so there is no once-per-run serve to receive the run-level union. It also consumes NO MCP definitions yet: the bridge (internal/antigravity/bridge.go) is a deliberate STUB whose Start returns ErrNotImplemented, so there is no resolving path to witness. Both axes are negative BY CONSTRUCTION rather than by omission — children 5/6 build the session behaviour and must then flip perSessionBundle on, adding the witnesses that prove it."
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
	case adapter.KindAntigravity:
		return adapterClass{
			installs: []adapterInstall{
				// Google's documented default: the `agy` launcher in ~/.local/bin. A LAUNCHER FILE, not a
				// directory, exactly like claude's ~/.local/bin/claude.
				{probe: join(".local", "bin", "agy"),
					fingerprint: []string{join(".local", "bin", "agy")}},
				// AND THE SYSTEM INSTALL. Google's launcher lands in ~/.local/bin on a user install and in
				// /usr/bin on a system one — measured on the reference host, where `agy` is /usr/bin/agy and
				// ~/.local/bin/agy does not exist. Declaring only the home path made the adapter
				// "unresolvable, not mountable" and it never worked in a runtime container. Only the
				// DECLARED path is mounted, so naming a system directory here mounts nothing but agy itself.
				{probe: "/usr/bin/agy", fingerprint: []string{"/usr/bin/agy"}},
				// agy's own state home. PRIVATE for the same reason ~/.claude is: the CLI writes it, but an
				// operator's credential/session store must not become writable from a worker.
				{probe: join(".gemini", "antigravity-cli"), dir: true, private: true,
					fingerprint: []string{join(".gemini", "antigravity-cli")}},
			},
			// BOTH AXES NEGATIVE AND EXPLAINED — see antigravityNoServeNote. The bridge is a stub, so
			// there is no resolving path to witness yet; children 5/6 flip this on with witnesses.
			perSessionBundle: false,
			runUnionAtServe:  false,
			note:             antigravityNoServeNote,
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
		// NEVER MOUNT A SYSTEM DIRECTORY BY THE PARENT-DIRECTORY RULE.
		//
		// This pass falls back to mounting whatever holds the resolved binary, which for a binary under
		// $HOME is the right grain. For one outside it — /usr/bin, /usr/local/bin — it would mount the
		// entire system bin directory read-only into a worker's container: hundreds of unrelated tools,
		// and a much wider surface than the adapter needs. A system install is declared explicitly in
		// classifyAdapter (see agy and claude), and that declaration mounts the BINARY.
		if !pathWithin(dir, home) {
			continue
		}
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
			st, err := os.Stat(in.probe) // follows symlinks
			if err != nil {
				continue
			}
			if in.dir && !st.IsDir() {
				continue
			}
			if !in.dir && st.IsDir() {
				continue
			}
			// No `,z` relabel yet lands it with the ingress slice (architecture note D-4); see
			// the same note at the daemon's -v loop in daemon.go createContainer.
			mode := ":ro"
			if in.rw {
				mode = ":rw"
			}
			args = append(args, "-v", src+":"+src+mode)
			// A SYMLINKED ADAPTER NEEDS ITS TARGET TOO.
			//
			// `st, err := os.Stat` above FOLLOWS the link, but the mount is made at the LINK'S path — and
			// a bind mount of a symlink does not carry what it points at. `claude` is exactly that shape:
			//   ~/.local/bin/claude -> ~/.local/share/claude/versions/2.1.319
			// so the container received a link into a directory that was never mounted, and running
			// `claude` failed with "not found" — the adapter was present on the host, declared in the
			// install table, and unusable in every runtime container.
			//
			// Mounting the resolved target at its own absolute path makes the existing link work. It is
			// mounted as the resolved directory, so a version bump needs no new declaration here.
			if real, rerr := filepath.EvalSymlinks(in.probe); rerr == nil && real != in.probe {
				if _, serr := os.Stat(real); serr == nil {
					args = append(args, "-v", real+":"+real+mode)
				}
			}
		}
	}
	return args
}

// pathWithin reports whether p is home itself or a descendant of it.
func pathWithin(p, home string) bool {
	if home == "" {
		return false
	}
	cp, ch := filepath.Clean(p), filepath.Clean(home)
	if cp == ch {
		return true
	}
	rel, err := filepath.Rel(ch, cp)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
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
	case adapter.KindAntigravity:
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
		// HOST-FIRST, EVERY KIND, NO SPECIAL CASES.
		//
		// The rule is one sentence: if the host has a CLI this container can execute, put its bin dir on
		// the PATH (ahead of /usr/local/bin, where an image-baked CLI would live). Otherwise fall back to
		// the image's CLI — and only if the image genuinely ships one.
		//
		// The old code branched per kind, and the `default:` arm trusted imageSuppliesAdapter, which
		// claimed the image shipped opencode when it ships nothing. That single wrong claim is what made
		// every runtime run fail to start.
		// THE ORDER IS THE OPERATOR'S: image first, host mount second.
		//
		// For opencode/claude/antigravity the image DOWNLOADS the adapter at build time, so the image's
		// CLI (/usr/local/bin) must stay ahead. The host bin dir is appended AFTER it, which is the
		// fallback role — a stale image then still works instead of being unusable.
		//
		// `orchicon` is the exception and the first entry in that sense: it is bind-mounted at
		// /usr/local/bin/orchicon at create time (never baked), so the host path IS the primary route.
		//
		// This function only contributes HOST dirs; /usr/local/bin is always on the image's own PATH
		// ahead of anything appended here, so appending in this order is what "image first" means.
		// THE CLI ADAPTERS ARE NAMED EXPLICITLY, and that is load-bearing rather than decorative:
		// adapter_source_witness_test.go asserts this function mentions both, because a kind that falls
		// through to nowhere keeps the image-only rule silently — which is exactly how `claude` became
		// unreachable from the container a second time (its symlink target was never mounted).
		//
		// The native kind is deliberately absent: it is bind-mounted by the daemon, not resolved.
		switch normalizedKind(kind) {
		case adapter.KindClaude, adapter.KindAntigravity, adapter.DefaultAdapterKind:
		default:
			continue
		}
		hostBin := adapter.ResolveBinary(hostGOOS, home, kind, os.Getenv)
		hostUsable, _ := adapter.PreferHostAdapterInContainer(hostGOOS, hostBin)
		if !hostUsable {
			// The host cannot serve this kind. Fine if the image bakes it — that is now the primary
			// route. If neither can, CheckAdaptersForContainerWith reports it with a remedy.
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
type imageAdapterKind struct {
	// kind is the adapter kind (adapter.DefaultAdapterKind, adapter.KindClaude, ...).
	kind string
	// bin is the executable NAME the image installs on PATH.
	bin string
}

// imageAdapters is the set the shipped images install, keyed by kind.
//
// THIS LIST WAS WRONG, AND IT BROKE EVERY RUNTIME RUN.
//
// It claimed the runtime image ships `opencode` and `claude`. Measured against
// orchicon-runtime:orchicon-dev:
//
//	$ command -v opencode    → absent
//	$ command -v claude      → absent
//
// The image bakes NEITHER. Both are mounted from the host at create time (see deploy/runtime/Dockerfile:
// the adapters are "mount, never bake", the same rule as the orchicon binary). Because this list said
// otherwise, adapterPathPrefixFor skipped the host bin dir under the `default:` arm — "the image has it" —
// so the container's PATH never contained ~/.opencode/bin. The supervisor then failed its very first
// step with `exec: "opencode": executable file not found in $PATH`, the handshake exited 1, and the run
// died at start with every step still `pending`.
//
// WHAT THIS LIST MEANS, AND WHAT IT IS NOT.
//
// It describes what the runtime IMAGES ARE BUILT TO PROVIDE — deploy/runtime/Dockerfile.dev and
// Dockerfile.gui both do:
//
//	ARG ORCHICON_ADAPTERS="opencode claude"
//	opencode) npm install -g "@opencode/cli@latest" ;;
//	claude)   npm install -g "@anthropic-ai/claude-code@latest" ;;
//
// That is the operator's design, stated plainly: opencode, claude and (eventually) antigravity are
// DOWNLOADED INTO THE IMAGE at build time, and the mounted host command is the FALLBACK. The `orchicon`
// binary is the reverse — prebaked/mounted, host as fallback.
//
// IT IS NOT A CLAIM ABOUT ANY PARTICULAR IMAGE ON DISK. Earlier tonight I emptied this list because one
// stale local build did not contain the adapters — and that was the wrong conclusion from the right
// measurement: the image was built 2026-10-09, three weeks BEFORE PR #882 added the download step on
// 2026-10-30. The code was right; the image was old. Emptying the list would have made the system
// permanently image-blind even after a correct rebuild.
//
// SO THE LIST STAYS, AND THE RULE IS: image first, host mount second. A host mount still wins when the
// image genuinely has nothing (see adapterPathPrefixFor), which is what keeps a stale image working
// instead of unusable.
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
		bin, err := resolveBin(kind)
		if err == nil && bin != "" {
			if ok, why := adapter.ContainerUsable(bin, "linux"); !ok {
				// The host has one, but a Linux container cannot run it. If the image does not bake a
				// replacement, that IS the problem and it must name the remedy.
				if _, inImage := imageSupplies(kind); !inImage {
					problems = append(problems, kind+": "+why)
				}
			}
			// A usable host install wins regardless of what the image ships: host-first.
			continue
		}
		// No host install. That is fine ONLY if the image really provides one; otherwise the container
		// will fail to start this adapter's CLI, which is better said here than discovered from a bare
		// "exit status 1" during the handshake.
		if _, inImage := imageSupplies(kind); inImage {
			continue
		}
		// THE NATIVE KIND IS NOT A DOWNLOADABLE ADAPTER, and must not be judged as one.
		//
		// `orchicon` is never resolved from PATH or from an install list: the daemon BIND-MOUNTS its own
		// executable at /usr/local/bin/orchicon at create time (daemon.go, createContainer), which it has
		// already validated before this check ever runs. It has no ORCHICON_*_BIN override and no image
		// entry — so falling through here reported the native kind as a missing adapter with an empty
		// remedy string, which would have been a false alarm on every single runtime run.
		if normalizedKind(kind) == adapter.KindOrchicon {
			continue
		}
		remedy := adapter.BinEnv(kind)
		where := "install it on the host"
		if remedy != "" {
			where = "install it on the host (or set " + remedy + ")"
		}
		problems = append(problems, kind+": no host install was found and the runtime image does not bake "+
			"this adapter — "+where+" so it can be mounted into the container")
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
