package runtime

import (
	"path/filepath"
	"strings"
)

// Engine identifies the container engine binary the image path drives.
//
// Orchicon's image path (build, inspect gate, rmi, pull, prune) is expressed
// entirely in engine-CLI verbs that Podman and Docker share, so the whole
// path is repointed by resolving ONE binary: every daemon operation already
// goes through Daemon.DockerBin. Engine carries the engine-specific bits a
// shared CLI cannot express — how the engine words "that image is not here"
// — and the human name used in logs and error text.
type Engine struct {
	// Bin is the executable invoked as the engine CLI ("podman", "docker",
	// or an absolute path).
	Bin string
	// Name is the human-readable engine name ("podman" | "docker"), used in
	// log lines and operator-facing error messages.
	Name string
}

// Engine names, as reported by Engine.Name.
const (
	EnginePodman = "podman"
	EngineDocker = "docker"
)

// DetectEngine resolves the container engine binary. Precedence:
//
//  1. the ORCHICON_CONTAINER_BIN override (operators pinning the engine, or
//     using a podman-docker shim under a non-standard name);
//  2. podman on PATH — the engine Orchicon ships for;
//  3. docker on PATH — still supported, so an existing Docker host keeps
//     working without configuration;
//  4. "docker" as a bare fallback.
//
// Case 4 is deliberately NOT an error: the daemon must start on a host whose
// engine is installed later (the image path then fails with an actionable
// engine-not-found error instead of the daemon refusing to boot).
func DetectEngine(override string, lookPath func(string) (string, error)) Engine {
	if o := strings.TrimSpace(override); o != "" {
		return Engine{Bin: o, Name: EngineName(o)}
	}
	if lookPath != nil {
		for _, name := range []string{EnginePodman, EngineDocker} {
			if _, err := lookPath(name); err == nil {
				return Engine{Bin: name, Name: name}
			}
		}
	}
	return Engine{Bin: EngineDocker, Name: EngineDocker}
}

// EngineName maps an engine binary (possibly an absolute path) to its
// canonical name. Unknown binaries report their basename, so an unusual
// override still logs something an operator can act on.
func EngineName(bin string) string {
	base := strings.ToLower(filepath.Base(strings.TrimSpace(bin)))
	switch {
	case base == "":
		return EngineDocker
	case strings.Contains(base, EnginePodman):
		return EnginePodman
	case strings.Contains(base, EngineDocker):
		return EngineDocker
	}
	return base
}

// IsAbsentOutput reports whether an engine command's output means "that
// image does not exist locally". The wording is the ONE place the engines
// differ in a way the image path cares about:
//
//   - docker rmi: "Error: No such image: foo:latest"
//   - podman rmi: "Error: foo:latest: image not known"
//   - podman/docker inspect of a missing ref: "...: image not known" /
//     "No such image"
//
// Matching is case-insensitive and deliberately narrow: it is used only to
// turn a failed removal into the idempotent "already gone" result.
func (e Engine) IsAbsentOutput(out string) bool {
	l := strings.ToLower(out)
	for _, s := range []string{"no such image", "image not known", "no such object"} {
		if strings.Contains(l, s) {
			return true
		}
	}
	return false
}
