package runtime

import (
	"os"
	"path/filepath"
	"strings"
)

// Engine is the container engine the runtime daemon drives. It is a
// BEHAVIOUR switch, not merely a binary name: the name a runtime container
// uses to reach a HOST-resident plane, and whether a CONTAINER-resident
// plane can derive its own address, both differ between Docker and Podman.
//
// Podman on macOS/Windows runs containers inside a VM (the "podman
// machine"), so the host and the engine's containers sit in different
// network namespaces: `host.docker.internal` is a Docker-only name,
// `host.containers.internal` is Podman's, and a rootless Podman container
// has no bridge IP at all. Every place in this package that used to assume
// the Docker topology goes through the helpers below, so the assumption is
// named once (docs/adr/0013-podman-runtime-boundary.md).
//
// The zero value is not meaningful: callers use engineKind(), which always
// resolves to a concrete engine (Docker is the shipped default).
type Engine string

const (
	// EngineDocker is Docker Engine / Docker Desktop. The shipped default.
	EngineDocker Engine = "docker"
	// EnginePodman is Podman, including the VM-backed podman machine on
	// macOS/Windows and rootless Linux Podman.
	EnginePodman Engine = "podman"
)

// EnvRuntimeEngine selects the engine EXPLICITLY. It wins over binary
// inference because the binary path does not identify the engine: the
// `podman-docker` package installs a `docker` shim in front of Podman, and
// Docker Desktop ships `docker` in front of a VM-backed engine.
const EnvRuntimeEngine = "ORCHICON_RUNTIME_ENGINE"

// EnvContainerBin overrides the engine CLI binary (e.g. an absolute podman
// path that is not on the daemon's PATH).
const EnvContainerBin = "ORCHICON_CONTAINER_BIN"

// EnvPlaneHostAlias overrides the in-container name that resolves to the
// host. Needed when neither engine default resolves — rootless Podman on a
// custom network, a remote engine, or an engine reached through a tunnel.
const EnvPlaneHostAlias = "ORCHICON_PLANE_HOST_ALIAS"

// engineKind resolves the container engine for this process. Resolution
// order:
//
//  1. ORCHICON_RUNTIME_ENGINE (explicit wins; unrecognised values fall
//     through so a typo cannot silently select the other engine's
//     networking).
//  2. The basename of ORCHICON_CONTAINER_BIN when it names podman.
//  3. Docker — the shipped default, which keeps every existing install on
//     the behaviour it already has.
func engineKind() Engine {
	if v := strings.ToLower(strings.TrimSpace(os.Getenv(EnvRuntimeEngine))); v != "" {
		switch v {
		case string(EnginePodman):
			return EnginePodman
		case string(EngineDocker):
			return EngineDocker
		}
	}
	if b := strings.TrimSpace(os.Getenv(EnvContainerBin)); b != "" {
		if strings.Contains(strings.ToLower(filepath.Base(b)), "podman") {
			return EnginePodman
		}
	}
	return EngineDocker
}

// EngineBinary resolves the engine CLI the daemon should exec. This is the
// single source the daemon's DockerBin field is built from
// (cmd/orchicon/runtime.go) — Daemon.DockerBin was hardcoded "docker",
// which is exactly what made Podman unusable despite the exec choke point
// being one line.
func EngineBinary() string {
	if b := strings.TrimSpace(os.Getenv(EnvContainerBin)); b != "" {
		return b
	}
	return string(engineKind())
}

// planeHostAlias is the name a runtime container uses to reach a
// HOST-resident plane. Docker: host.docker.internal (mapped to the host by
// --add-host, see hostGatewayArgs). Podman: host.containers.internal, which
// Podman injects into every container.
//
// ORCHICON_PLANE_HOST_ALIAS overrides both — an operator escape hatch for an
// engine/network shape where neither default resolves.
func planeHostAlias() string {
	if v := strings.TrimSpace(os.Getenv(EnvPlaneHostAlias)); v != "" {
		return v
	}
	if engineKind() == EnginePodman {
		return "host.containers.internal"
	}
	return "host.docker.internal"
}

// containerResidencyNeedsOverride reports whether a CONTAINER-RESIDENT plane
// cannot derive its own address and therefore REQUIRES an explicit
// ORCHICON_PLANE_PUBLIC_URL.
//
// Rootless Podman containers get no bridge IP (slirp4netns/pasta), so
// containerIPAddress() returns "" and the historical Docker gateway
// fallback (172.17.0.1) is not this engine's gateway at all — emitting it
// would make every plane-channel call fail somewhere far from the cause.
// Docker keeps the historical guard.
func containerResidencyNeedsOverride() bool {
	return engineKind() == EnginePodman
}

// hostGatewayArgs returns the `docker run` args that make planeHostAlias()
// resolvable from inside a runtime container.
//
// Docker needs the explicit host-gateway mapping (lifecycle.go
// planePublicURL's host-residency fallback depends on it). Podman injects
// host.containers.internal natively, so no arg is added — and passing
// --add-host host-gateway is avoided there, since older Podman builds
// reject the special value and would fail container creation.
func hostGatewayArgs() []string {
	if engineKind() == EnginePodman {
		return nil
	}
	return []string{"--add-host", planeHostAlias() + ":host-gateway"}
}
