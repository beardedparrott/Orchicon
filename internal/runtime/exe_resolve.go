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
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ContainerExeEnv names the linux binary the daemon mounts into runtime containers.
const ContainerExeEnv = "ORCHICON_RUNTIME_CONTAINER_EXE"

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
// On a Linux host that is the daemon's own binary, unchanged from today. On Windows it must be a linux/amd64
// build, resolved from (in order) the explicit override, a sibling of the daemon's own executable, or nothing —
// in which case the returned error names the file to place and why.
//
// The returned path is a HOST path; the caller maps it to the engine's view separately (pathmap.go), because the
// two questions are independent: WHICH file, and HOW the engine addresses it.
func ResolveContainerExe(goos, hostExe, override string) (string, error) {
	if override = strings.TrimSpace(override); override != "" {
		if fi, err := os.Stat(override); err != nil || fi.IsDir() {
			return "", fmt.Errorf("%s points at %q, which is not a readable file — it must be a linux/amd64 "+
				"orchicon build, because runtime containers are Linux containers", ContainerExeEnv, override)
		}
		return override, nil
	}
	if goos != "windows" {
		// Linux: the daemon's own binary IS the container binary — the original design, unchanged.
		return hostExe, nil
	}
	// WINDOWS: the daemon's own executable is a PE file and cannot run inside a Linux container.
	if hostExe == "" {
		return "", fmt.Errorf("cannot locate a linux orchicon binary for runtime containers: the daemon's own " +
			"executable path is unknown, and its binary cannot be used — runtime containers are Linux")
	}
	dir := filepath.Dir(hostExe)
	for _, name := range containerExeSiblingNames {
		for _, candidate := range []string{filepath.Join(dir, name), filepath.Join(dir, name+".exe")} {
			if fi, err := os.Stat(candidate); err == nil && !fi.IsDir() {
				return candidate, nil
			}
		}
	}
	return "", fmt.Errorf("runtime containers need a linux/amd64 orchicon binary, and none was found beside %s "+
		"(looked for %s). A Windows executable cannot run inside a Linux container, so the container's supervisor "+
		"cannot start without one — re-run the Orchicon installer with containers enabled (`install.ps1 "+
		"-Containers`), or place a linux build at one of those names and set %s to it",
		hostExe, strings.Join(containerExeSiblingNames, ", "), ContainerExeEnv)
}
