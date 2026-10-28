package runtime

// pathmap.go — EXPRESSING A HOST PATH THE WAY THE CONTAINER ENGINE'S VM SEES IT.
//
// THE PROBLEM, from podman's own documentation rather than from inference:
//
//	podman-machine(1): "Podman on MacOS and Windows requires a virtual machine. This is because containers are
//	Linux — containers do not run on any other OS because containers' core functionality are tied to the Linux
//	kernel."
//
//	podman-machine-init(1), the WSL note: "Since all drives are mounted at /mnt on startup by default in WSL,
//	passing --volume is redundant and has no effect. The host home directory for a C: drive will be mounted at
//	/mnt/c/Users/<my username>."
//
// So on Windows the engine runs in a Linux VM and the filesystem it shares is addressed as /mnt/c/..., NOT as
// C:\... . Our daemon passed the Windows form straight through (`MountSpec{Source: projectDir, Dest: projectDir}`
// with projectDir = `C:\projects\app`), so every container create would have been handed a source path the VM
// cannot resolve.
//
// WHY A MAPPER AND NOT A STRING REPLACE AT EACH CALL SITE. Mounts are built in three separate places — the
// requested mounts, the adapter/git host mounts, and the executable mount — and a fourth would be added by
// whoever needs one next. One mapper, applied at the argv boundary, is the only shape where "every path the
// engine sees went through the mapping" is true by construction.
//
// WHAT IT IS NOT. It is not a general path translator: it does not resolve symlinks, follow junctions, or
// canonicalise case. It maps the two forms the engine's VM understands, and REFUSES (returns the input
// untouched with ok=false) anything it cannot map confidently — because silently passing a path the VM cannot
// resolve produces exactly the class of failure this whole file exists to prevent.

import (
	"os"
	"path/filepath"
	stdruntime "runtime"
	"strings"
)

// PathMode is how a host path must be expressed for the container engine on this host.
type PathMode string

const (
	// PathModeIdentity passes paths through unchanged: the engine shares the host's filesystem namespace.
	// True for Linux, and for a non-VM provider.
	PathModeIdentity PathMode = "identity"

	// PathModeWSL maps a Windows drive path onto the WSL mount form: `C:\projects\app` -> `/mnt/c/projects/app`.
	// This is podman machine's DEFAULT provider on Windows (podman-machine(1)'s provider table marks wsl with the
	// default asterisk), and its documented share.
	PathModeWSL PathMode = "wsl"

	// PathModeWindowsShare leaves the path in Windows form. A Hyper-V machine resolves volumes from the SERVER
	// side rather than the client, so there is no /mnt mapping to apply; the Windows form is what the engine
	// receives. It is listed explicitly rather than folded into identity because the two differ in MEANING: a
	// future reader must not conclude that a Windows path reaching a Linux VM was simply not thought about.
	PathModeWindowsShare PathMode = "windows-share"
)

// PathMapper maps a host path into the form the engine's VM expects.
type PathMapper struct {
	mode PathMode
}

// NewPathMapper builds a mapper for a platform and a machine provider.
//
// provider is the machine's VM type (`wsl`, `hyperv`, `applehv`, `libkrun`, `qemu`) when a caller knows it.
// Empty means "not known", and on Windows that falls back to wsl — podman's own DEFAULT provider, per
// podman-machine(1)'s provider table — because the alternative (leaving paths unmapped) silently produces a
// container create whose engine cannot resolve its own project directory.
//
// WHAT WE DO NOT DO: query the provider ourselves. `podman machine list --format json` does expose a VMType,
// and internal/containerengine deliberately reads only the fields it uses so a 4.x/5.x shape change cannot
// break it. Depending on that field here would make the PATH MAPPING — which every container create needs —
// fail on a podman whose listing omits it. The default is right on Windows and identity is right everywhere
// else, so the knowledge-below-the-default case does not arise. ORCHICON_RUNTIME_PATH_MODE overrides it for
// the one case that does: a Windows host whose podman machine uses the hyperv provider.
func NewPathMapper(goos, provider string) *PathMapper {
	p := strings.ToLower(strings.TrimSpace(provider))
	switch goos {
	case "windows":
		switch p {
		case "hyperv":
			return &PathMapper{mode: PathModeWindowsShare}
		case "", "wsl":
			return &PathMapper{mode: PathModeWSL}
		default:
			// An unknown provider on Windows: prefer the mapping podman defaults to rather than passing a
			// form that certainly will not resolve in a WSL-backed machine.
			return &PathMapper{mode: PathModeWSL}
		}
	default:
		// Linux: the engine shares the kernel's filesystem. macOS with podman machine shares $HOME at
		// IDENTICAL guest paths, so the host form is already correct (and the machine-init docs' 9p note is
		// about security models, not path renames).
		return &PathMapper{mode: PathModeIdentity}
	}
}

// PathModeEnv overrides the derived path mode.
//
// IT EXISTS FOR ONE CASE: a Windows host running podman machine with the hyperv provider, which resolves volumes
// server-side and therefore needs Windows-form paths rather than /mnt ones. Deriving that automatically would mean
// depending on a `podman machine list` field this codebase deliberately does not read (see NewPathMapper), so the
// non-default provider is an explicit, documented override instead of a silent guess.
const PathModeEnv = "ORCHICON_RUNTIME_PATH_MODE"

// NewPathMapperFromEnv builds the mapper the daemon uses: the platform default, overridable by PathModeEnv.
func NewPathMapperFromEnv(goos string) *PathMapper {
	m := NewPathMapper(goos, "")
	switch strings.ToLower(strings.TrimSpace(os.Getenv(PathModeEnv))) {
	case string(PathModeWSL):
		return &PathMapper{mode: PathModeWSL}
	case string(PathModeWindowsShare):
		return &PathMapper{mode: PathModeWindowsShare}
	case string(PathModeIdentity):
		return &PathMapper{mode: PathModeIdentity}
	}
	return m
}

// Mode reports the mapper's mode, for logging at daemon start.
func (m *PathMapper) Mode() PathMode {
	if m == nil {
		return PathModeIdentity
	}
	return m.mode
}

// Map returns the path as the engine's VM must see it.
//
// IDENTITY AND SHARE MODES RETURN THE INPUT UNCHANGED. Only the WSL mode rewrites, and only for a path it can
// confidently rewrite — an absolute Windows drive path. A UNC path (`\\server\share\...`) is deliberately left
// alone: it is not on a drive WSL mounts at /mnt, so inventing `/mnt/unc/...` would be worse than reporting the
// path as-is and letting the engine's own error name it.
func (m *PathMapper) Map(p string) string {
	if m == nil || m.mode != PathModeWSL {
		return p
	}
	if p == "" {
		return p
	}
	// Already in the guest's form (a caller that pre-mapped, or a path that came FROM the guest).
	if strings.HasPrefix(p, "/") {
		return p
	}
	drive, rest, ok := splitDrivePath(p)
	if !ok {
		return p
	}
	// `/mnt/c/projects/my app` — the drive letter lowercased, and the remainder re-slashified. Spaces and other
	// characters are left EXACTLY as they are: the engine takes the mount as a single argv element
	// (`mountArgv` builds `source:dest:opts` as one string), so nothing here needs quoting or escaping.
	return "/mnt/" + strings.ToLower(drive) + "/" + rest
}

// splitDrivePath splits `C:\a\b` or `C:/a/b` into the drive letter and a slash-separated remainder.
//
// DRIVE-RELATIVE PATHS (`C:foo`) ARE REFUSED, because they mean "relative to the current directory on drive C"
// — a third form with no /mnt equivalent. Only an absolute path (`C:\` or `C:/`) is mappable.
func splitDrivePath(p string) (drive, rest string, ok bool) {
	if len(p) < 3 {
		return "", "", false
	}
	c := p[0]
	if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')) {
		return "", "", false
	}
	if p[1] != ':' {
		return "", "", false
	}
	sep := p[2]
	if sep != '\\' && sep != '/' {
		return "", "", false
	}
	rest = strings.ReplaceAll(p[3:], "\\", "/")
	// Collapse any doubled separators the source may have had, so `C:\\a` and `C:\a` agree.
	for strings.Contains(rest, "//") {
		rest = strings.ReplaceAll(rest, "//", "/")
	}
	rest = strings.TrimPrefix(rest, "/")
	if rest == "" {
		return string(c), "", true
	}
	return string(c), rest, true
}

// MapAll maps every path in a slice, preserving order and returning a NEW slice so the caller's request is not
// mutated (the request may be retried, logged, or re-created from the same value).
func (m *PathMapper) MapAll(paths []string) []string {
	if m == nil || m.mode != PathModeWSL {
		return paths
	}
	out := make([]string, len(paths))
	for i, p := range paths {
		out[i] = m.Map(p)
	}
	return out
}

// mapMounts returns a copy of the mounts with BOTH sides mapped.
//
// BOTH SIDES, because they are different things that happen to hold the same string today: Source is a path on
// the host the engine must resolve, Dest is a path INSIDE the container. When the plane sends
// `C:\projects\app` for both, the engine's source must become `/mnt/c/projects/app` — and the container-side
// destination must too, because the in-container `opencode` serve and every bash/file tool resolve the project
// through that path, and the plane's `ProjectDir` is what it reports back.
func mapMounts(m *PathMapper, mounts []MountSpec) []MountSpec {
	if m == nil || m.mode != PathModeWSL || len(mounts) == 0 {
		return mounts
	}
	out := make([]MountSpec, len(mounts))
	for i, ms := range mounts {
		out[i] = MountSpec{
			Source: m.Map(ms.Source),
			Dest:   m.Map(ms.Dest),
			RO:     ms.RO,
		}
	}
	return out
}

// mountDestDir returns a path's directory as the guest sees it — used where a guest-side file must be placed
// beside a host path (the private adapter homes' credential file).
func (m *PathMapper) mountDestDir(hostPath string) string {
	return m.Map(filepath.Dir(hostPath))
}

// mapMountArgv rewrites the SOURCE side of an already-built `-v src:dst[:opts]` argv block.
//
// adapterHostMounts and friends return finished `-v` argument pairs, because they are the one declaration the
// mount-never-bake guard also reads. Rebuilding those pairs here would duplicate a shape that must not drift, so
// the mapper edits only what it must: the host side, which is the part the engine's VM has to resolve. The
// destination is left exactly as the declaration wrote it — those are guest paths by construction.
//
// An argv element that is not a `-v` source (the flag itself, a tmpfs value, an `-e KEY=VALUE`) is passed
// through untouched, so the whole block can be fed in without the caller having to know its shape.
func mapMountArgv(m *PathMapper, argv []string) []string {
	if m == nil || m.mode != PathModeWSL || len(argv) == 0 {
		return argv
	}
	out := make([]string, len(argv))
	for i, a := range argv {
		// Only the element IMMEDIATELY after "-v" is a spec, and only its first field is a host path.
		if i > 0 && argv[i-1] == "-v" {
			out[i] = mapMountSpecString(m, a)
			continue
		}
		out[i] = a
	}
	return out
}

// mapMountSpecString maps one `src:dst[:opts]` string, BOTH SIDES.
//
// BOTH, and the first version of this got that wrong: it mapped only the source and left the destination as
// `C:\Users\op\.gitconfig`. The declaration is written with the host path on both sides, and a Windows path is
// not a valid container destination — the mount would be created at a path inside the container literally named
// `C:\...`, so the in-container serve and every file tool would look in the wrong place. Source and destination
// are different things that happen to hold the same string; both have to move.
//
// THE SPLIT IS ON THE FIRST COLON THAT IS NOT A DRIVE LETTER, and that is not a simplification: a Windows source
// begins `C:` and holds a colon of its own, so splitting on every colon (or on the last) mangles exactly the case
// this exists for. The destination always begins '/' — a guest path — which is what identifies the boundary.
//
// It is idempotent: a declaration already carrying guest paths is returned unchanged.
func mapMountSpecString(m *PathMapper, spec string) string {
	if spec == "" {
		return spec
	}
	src, rest := splitMountSpec(spec)
	if rest == "" {
		return m.Map(spec)
	}
	// rest is `dst[:opts]`; split the destination from the options the same way (dst is absolute guest form).
	dst, opts := splitDestOpts(rest)
	out := m.Map(src) + ":" + m.Map(dst)
	if opts != "" {
		out += ":" + opts
	}
	return out
}

// splitDestOpts splits `dst[:opts]` where dst may ITSELF be a Windows path (the git and gh mounts are declared
// with the same host path on both sides), so the drive-letter colon must be skipped here too — a first version
// that did not split `C:\Users\...:ro` at the right colon and produced a destination of "C".
func splitDestOpts(rest string) (dst, opts string) {
	start := 0
	if len(rest) >= 3 && rest[1] == ':' &&
		((rest[0] >= 'a' && rest[0] <= 'z') || (rest[0] >= 'A' && rest[0] <= 'Z')) &&
		(rest[2] == '\\' || rest[2] == '/') {
		start = 2
	}
	for i := start; i < len(rest); i++ {
		if rest[i] == ':' {
			return rest[:i], rest[i+1:]
		}
	}
	return rest, ""
}

// splitMountSpec splits `src:dst[:opts]` where src may itself contain `C:`.
//
// THE RULE IS "THE FIRST COLON THAT IS NOT THE DRIVE LETTER", deliberately NOT "the first colon followed by /".
// The destination in a real declaration is often the SAME Windows path as the source
// (`C:\Users\op\.gitconfig:C:\Users\op\.gitconfig:ro` — how the git and gh mounts are written), so a
// followed-by-'/' test finds no boundary at all in exactly the case this function exists for. The first version
// did that and silently returned no split, leaving the destination unmapped.
//
// Returns ("", "") only when no colon exists outside a drive letter — a bare source, or a named volume.
func splitMountSpec(spec string) (src, rest string) {
	// A leading drive letter ("C:" + separator) is part of the SOURCE, so start after it.
	start := 0
	if len(spec) >= 3 && spec[1] == ':' &&
		((spec[0] >= 'a' && spec[0] <= 'z') || (spec[0] >= 'A' && spec[0] <= 'Z')) &&
		(spec[2] == '\\' || spec[2] == '/') {
		start = 2
	}
	for i := start; i < len(spec); i++ {
		if spec[i] == ':' {
			return spec[:i], spec[i+1:]
		}
	}
	return "", ""
}

// GOOS reports the host platform, so cmd/orchicon does not need its own runtime import alias for one call.
func GOOS() string { return stdruntime.GOOS }
