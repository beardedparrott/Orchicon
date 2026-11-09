package runtime

// exe_resolve.go — WHICH BINARY RUNS INSIDE THE CONTAINER.

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

// ErrNoContainerBinary marks a create failure that WAITING CANNOT FIX: this host has no linux
// binary to mount, or the one it has is not one a Linux kernel can execute.
var ErrNoContainerBinary = errors.New("no usable linux container binary on this host")

// ContainerBinaryErrCode is the wire code for ErrNoContainerBinary. It rides beside "error" in the daemon's
// JSON error body, which the client already decodes as map[string]string — so the field is additive.
const ContainerBinaryErrCode = "no_container_binary"

// containerExeSiblingNames are the file names checked beside the daemon's own executable, in order.
var containerExeSiblingNames = []string{
	"orchicon-linux-amd64",
	"orchicon-linux",
	"orchicon_linux_amd64",
}

// ResolveContainerExe decides which executable the daemon bind-mounts into runtime containers.
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

// containerExeSearchDirs is where a linux build is looked for, in order: beside the INVOKING
// executable (where install.ps1 puts it, and where an operator naturally drops one), then
// <dataDir>/bin.
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

// containerExeRemedy is the actionable half of the refusal: what to obtain and where to put it. IT
// NAMES A DURABLE LOCATION, NEVER THE DAEMON'S OWN PATH.
func containerExeRemedy(dataDir string) string {
	where := "<data dir>/bin"
	if dataDir != "" {
		where = filepath.Join(dataDir, "bin")
	}
	return fmt.Sprintf("Re-run the Orchicon installer with containers enabled (install.ps1 on Windows, "+
		"install.sh elsewhere) and it downloads the linux build for you; or place one at %s; or point %s at one",
		filepath.Join(where, containerExeSiblingNames[0]), ContainerExeEnv)
}

// isELFFile reports whether path begins with the ELF magic.
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
