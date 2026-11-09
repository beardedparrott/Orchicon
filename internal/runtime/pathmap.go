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
	"path"
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

	// PathModeWSL maps a Windows drive path onto the WSL mount form: `C:\projects\app` ->
	// `/mnt/c/projects/app`.
	PathModeWSL PathMode = "wsl"

	// PathModeWindowsShare leaves the path in Windows form.
	PathModeWindowsShare PathMode = "windows-share"
)

// PathMapper maps a host path into the form the engine's VM expects.
type PathMapper struct {
	mode PathMode
}

// NewPathMapper builds a mapper for a platform and a machine provider. WHAT WE DO NOT DO: query the
// provider ourselves.
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
		// Linux: the engine shares the kernel's filesystem.
		return &PathMapper{mode: PathModeIdentity}
	}
}

// PathModeEnv overrides the derived path mode.
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
	// `/mnt/c/projects/my app` — the drive letter lowercased, and the remainder re-slashified.
	return "/mnt/" + strings.ToLower(drive) + "/" + rest
}

// splitDrivePath splits `C:\a\b` or `C:/a/b` into the drive letter and a slash-separated remainder.
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

// Unmap is Map in reverse: a GUEST path becomes the HOST path it was mounted from.
func (m *PathMapper) Unmap(p string) (string, bool) {
	if m == nil || m.mode != PathModeWSL {
		return p, false
	}
	const prefix = "/mnt/"
	if len(p) < len(prefix)+2 || !strings.HasPrefix(p, prefix) {
		return p, false
	}
	drive := p[len(prefix)]
	if drive < 'a' || drive > 'z' {
		return p, false
	}
	rest := p[len(prefix)+1:]
	// rest is now "/<...>" — the path after the drive letter.
	if len(rest) == 0 || rest[0] != '/' {
		return p, false
	}
	rest = rest[1:]
	if rest == "" {
		return strings.ToUpper(string(drive)) + ":\\", true
	}
	return strings.ToUpper(string(drive)) + ":\\" + strings.ReplaceAll(rest, "/", "\\"), true
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

// withinAllowedRootsIn reports whether path is inside one of the allowed roots, COMPARING IN THE ENGINE'S VIEW.
//
// THE BUG THIS FIXES. The allowlist is a security boundary and it was checked against the HOST form of the path
// (`C:\projects\app`) while the mount handed to the engine is the GUEST form (`/mnt/c/projects/app`). Two ways
// that goes wrong, both silent:
//
//   - the roots are the documented default `%USERPROFILE%\projects` and the project is on `C:\projects`, so the
//     check correctly refuses — but the operator already saw the fetch succeed, and the refusal names a path they
//     never typed;
//   - worse, on Windows a host path and a guest path have no string relationship, so `strings.HasPrefix` can
//     never match a mapped mount against a host root. Every container create would be refused on Windows once
//     mapping was added, no matter how the roots were configured.
//
// So BOTH sides are mapped before comparing: the path being checked, and each configured root. On Linux and macOS
// the mapping is identity, so this is exactly the old comparison.
func (d *Daemon) withinAllowedRootsIn(path string) bool {
	if d == nil {
		return false
	}
	return withinRootsIn(d.Paths, path, d.AllowedRoots)
}

// withinRootsIn is the ONE comparison, over an explicit root set.
func withinRootsIn(m *PathMapper, hostPath string, roots []string) bool {
	if len(roots) == 0 {
		return false
	}
	// BOTH SIDES GO THROUGH THE SAME TWO STEPS, IN THE SAME ORDER: normalise as a HOST path, then map to the
	// ENGINE's form. Doing them in the other order, or normalising only one side, is the bug this replaces.
	guest := guestForm(m, hostPath)
	for _, root := range roots {
		r := guestForm(m, root)
		if guest == r || strings.HasPrefix(guest, r+"/") {
			return true
		}
	}
	return false
}

// guestForm is ONE path's engine-side form, normalised correctly for the OS it is ABOUT rather than the OS the
// binary happens to be running on.
//
// THE BUG THIS FIXES IS WINDOWS-ONLY AND MY OWN. This function used to be
//
//	filepath.Clean(m.Map(root))     // root side: cleaned, guest form
//	m.Map(path)                     // path side: NEVER cleaned
//
// On Windows, filepath.Clean applies WINDOWS rules to a GUEST path: `/mnt/c/projects/weatherapp` has no drive
// letter, so Clean treats it as drive-RELATIVE and rewrites the separators, giving
// `\mnt\c\projects\weatherapp`. The path side stayed `/mnt/c/projects/weatherapp`. The two could never be
// equal, and `HasPrefix(guest, "\mnt\...")` could never match either — so EVERY mount was refused on Windows
// while both sides were correct in their own terms. MEASURED, from the operator's own log, with the project
// directory plainly present in the allowlist it printed:
//
//	mount source outside allowed roots: C:\projects\weatherapp
//	    (as the container engine sees it: /mnt/c/projects/weatherapp;
//	     allowed roots: C:\Users\operator\projects, C:\projects\weatherapp ...)
//
// No value of ORCHICON_RUNTIME_ALLOWED_ROOTS could have fixed it, including C:\.
//
// THE RULE: a HOST path is normalised with filepath (whose rules are the host's, correctly); a GUEST path is
// normalised with `path`, which is slash-only by definition. Clean before mapping, and never clean after —
// the guest form belongs to a Linux VM and must be evaluated by Linux path rules whatever platform reads it.
func guestForm(m *PathMapper, hostPath string) string {
	if hostPath == "" {
		return ""
	}
	// 1. Host normalisation, with the HOST's rules. This is the only place filepath is correct.
	clean := filepath.Clean(hostPath)
	// 2. Map into the engine's namespace. Identity on Linux/macOS, so nothing below changes there.
	mapped := m.Map(clean)
	if m == nil || m.mode != PathModeWSL {
		return mapped
	}
	// 3. GUEST normalisation — slash semantics only. path.Clean cannot turn "/mnt/c/x" into "\mnt\c\x",
	// because it has no notion of a platform separator.
	return path.Clean(mapped)
}

// mapMountArgv rewrites the SOURCE side of an already-built `-v src:dst[:opts]` argv block.
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

// splitDestOpts splits `dst[:opts]` where dst may ITSELF be a Windows path (the git and gh mounts
// are declared with the same host path on both sides).
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
