package runtime

// exe_resolve.go — WHICH BINARY RUNS INSIDE THE CONTAINER.
//
// The runtime container is a LINUX container (deploy/runtime/Dockerfile: FROM debian:bookworm-slim) and it
// starts `orchicon runtime-supervisor`, while the daemon drives it with `orchicon runtime-client`. The base image
// deliberately does NOT bake the binary in — the daemon bind-mounts its own executable at
// /usr/local/bin/orchicon when it creates the container, so a rebuilt daemon needs no image rebuild.
//
// THAT DESIGN IS CORRECT ON A LINUX HOST AND IMPOSSIBLE ON WINDOWS. "The daemon's own executable" there is a
// Windows PE file, so the mount puts a Windows binary at /usr/local/bin/orchicon inside a Linux container. The
// container's supervisor cannot start, so the runtime never becomes ready — which is the shape of the Windows
// runtime failure this file exists to fix.
//
// podman/Terminal behaviour is not at fault and no path mapping can repair it: the container needs a binary the
// kernel inside it can execute, and a Windows executable is never that. So the daemon needs a SECOND executable
// — a linux/amd64 build of the same version — and it must say so plainly when one is missing rather than letting
// the container fail to start with the engine's own opaque message.

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// ContainerExeEnv names the linux binary the daemon mounts into runtime containers.
const ContainerExeEnv = "ORCHICON_RUNTIME_CONTAINER_EXE"

// ErrNoContainerBinary marks a create failure that WAITING CANNOT FIX: this host has no linux binary to mount,
// or the one it has is not one a Linux kernel can execute.
//
// It matters because the run-start serve gate retries a create every two seconds for up to two minutes. A
// missing binary is a property of the host, so every retry re-emits the same refusal and the operator reads a
// wall of identical errors. The distinction has to survive the daemon's HTTP boundary, which is why it travels
// as a CODE in the JSON error body (see ContainerBinaryErrCode) rather than only as a wrapped Go error.
var ErrNoContainerBinary = errors.New("no usable linux container binary on this host")

// ContainerBinaryErrCode is the wire code for ErrNoContainerBinary. It rides beside "error" in the daemon's
// JSON error body, which the client already decodes as map[string]string — so the field is additive.
const ContainerBinaryErrCode = "no_container_binary"

// containerExeSiblingNames are the file names checked beside the daemon's own executable, in order.
//
// The installer places the linux build next to the Windows binary under the first of these, so the default needs
// no configuration at all; the others tolerate a hand-downloaded copy.
var containerExeSiblingNames = []string{
	"orchicon-linux-amd64",
	"orchicon-linux",
	"orchicon_linux_amd64",
}

// ResolveContainerExe decides which executable the daemon bind-mounts into runtime containers.
//
// On a LINUX host that is the daemon's own binary, unchanged from the original design. Everywhere else the
// daemon's own file is the wrong platform — a PE on Windows, a Mach-O on macOS — so a linux/amd64 build is
// resolved from (in order) the explicit override, a sibling of the invoking executable, then <dataDir>/bin.
//
// macOS IS INCLUDED DELIBERATELY. It used to take the Linux branch, so a Mach-O was handed to the container
// entrypoint and the ELF check refused every create: container mode could not work on a Mac at all.
//
// The returned path is a HOST path; the caller maps it to the engine's view separately (pathmap.go), because
// the two questions are independent: WHICH file, and HOW the engine addresses it.
func ResolveContainerExe(goos, hostExe, override, dataDir string) (string, error) {
	if override = strings.TrimSpace(override); override != "" {
		if fi, err := os.Stat(override); err != nil || fi.IsDir() {
			return "", fmt.Errorf("%s points at %q, which is not a readable file — it must be a linux/amd64 "+
				"orchicon build, because runtime containers are Linux containers", ContainerExeEnv, override)
		}
		return override, nil
	}
	if goos == "linux" {
		// Linux: the daemon's own binary IS the container binary — the original design, unchanged.
		return hostExe, nil
	}
	dirs := containerExeSearchDirs(hostExe, dataDir)
	if len(dirs) == 0 {
		return "", fmt.Errorf("cannot locate a linux orchicon binary for runtime containers: the daemon's own "+
			"executable path is unknown, and its %s binary cannot be used — runtime containers are Linux",
			binaryKind(goos))
	}
	for _, dir := range dirs {
		for _, name := range containerExeSiblingNames {
			for _, candidate := range []string{filepath.Join(dir, name), filepath.Join(dir, name+".exe")} {
				if fi, err := os.Stat(candidate); err == nil && !fi.IsDir() {
					return candidate, nil
				}
			}
		}
	}
	return "", fmt.Errorf("runtime containers need a linux/amd64 orchicon binary: this host's own executable "+
		"is a %s file, which cannot run inside a Linux container. None was found in %s (looked for %s). %s",
		binaryKind(goos), strings.Join(dirs, " or "), strings.Join(containerExeSiblingNames, ", "),
		containerExeRemedy(dataDir))
}

// containerExeSearchDirs is where a linux build is looked for, in order: beside the INVOKING executable
// (where install.ps1 puts it, and where an operator naturally drops one), then <dataDir>/bin.
//
// THE SECOND DIRECTORY IS THE DURABLE ONE. The daemon self-copies itself to a stable path under the socket
// dir — %TEMP%\orchicon-runtime on Windows — and a temp cleaner can take that away; the data dir is the
// per-user state dir and survives. Empty or duplicate entries are dropped, so a host with no data dir simply
// searches one place.
func containerExeSearchDirs(hostExe, dataDir string) []string {
	var dirs []string
	seen := map[string]bool{}
	add := func(dir string) {
		if dir = strings.TrimSpace(dir); dir == "" || seen[dir] {
			return
		}
		seen[dir] = true
		dirs = append(dirs, dir)
	}
	if hostExe != "" {
		add(filepath.Dir(hostExe))
	}
	if dataDir != "" {
		add(filepath.Join(dataDir, "bin"))
	}
	return dirs
}

// binaryKind names an executable format for an error message. It takes the platform rather than reading GOOS:
// the message is about the platform being RESOLVED, which is not necessarily the one running this code.
func binaryKind(goos string) string {
	switch goos {
	case "windows":
		return "Windows PE"
	case "darwin":
		return "macOS Mach-O"
	default:
		return goos
	}
}

// containerExeRemedy is the actionable half of the refusal: what to obtain and where to put it.
//
// IT NAMES A DURABLE LOCATION, NEVER THE DAEMON'S OWN PATH. On Windows the daemon rewrites its executable path
// to the %TEMP% self-copy, so naming that path told the operator to fix up a directory Windows Storage Sense
// deletes — and it is not where the installer put the file either.
func containerExeRemedy(dataDir string) string {
	where := "<data dir>/bin"
	if dataDir != "" {
		where = filepath.Join(dataDir, "bin")
	}
	return fmt.Sprintf("Re-run the Orchicon installer with containers enabled (install.ps1 on Windows, "+
		"install.sh elsewhere) and it downloads the linux build for you; or place one at %s; or point %s at one",
		filepath.Join(where, containerExeSiblingNames[0]), ContainerExeEnv)
}

// isELFFile reports whether path begins with the ELF magic. It is the cheap, format-level check that catches a
// PE (or a Mach-O) reaching the container entrypoint, which is the failure this file exists to prevent.
//
// It reads FOUR BYTES, so it is safe to call on a hot path and cannot be fooled by a filename: an override
// pointing at a stray copy of the Windows build is caught here rather than in a readiness timeout.
func isELFFile(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	var magic [4]byte
	if _, err := io.ReadFull(f, magic[:]); err != nil {
		return false
	}
	return magic[0] == 0x7f && magic[1] == 'E' && magic[2] == 'L' && magic[3] == 'F'
}
