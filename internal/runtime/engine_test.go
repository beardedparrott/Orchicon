package runtime

// The engine seam (engine.go). Podman support is a BEHAVIOUR switch, not a
// binary swap: the host alias a runtime container uses to reach a
// host-resident plane and whether a container-resident plane can derive its
// own address both differ from Docker. An unrecognised
// ORCHICON_RUNTIME_ENGINE must never silently select the other engine's
// networking, and the Docker default must be untouched so every existing
// install keeps its behaviour.

import (
	"strings"
	"testing"
)

// clearEngineEnv neutralises every input the engine seam reads, then applies
// the caller's overrides.
func clearEngineEnv(t *testing.T, kv ...string) {
	t.Helper()
	t.Setenv(EnvRuntimeEngine, "")
	t.Setenv(EnvContainerBin, "")
	t.Setenv(EnvPlaneHostAlias, "")
	for i := 0; i+1 < len(kv); i += 2 {
		t.Setenv(kv[i], kv[i+1])
	}
}

func TestEngineKind(t *testing.T) {
	t.Run("docker is the default", func(t *testing.T) {
		clearEngineEnv(t)
		if got := engineKind(); got != EngineDocker {
			t.Fatalf("engineKind() = %q, want docker", got)
		}
	})
	t.Run("explicit podman", func(t *testing.T) {
		clearEngineEnv(t, EnvRuntimeEngine, "podman")
		if got := engineKind(); got != EnginePodman {
			t.Fatalf("engineKind() = %q, want podman", got)
		}
	})
	t.Run("explicit wins over the binary name", func(t *testing.T) {
		// podman-docker ships a `docker` shim in front of Podman: the binary
		// name is NOT evidence of the engine.
		clearEngineEnv(t, EnvRuntimeEngine, "docker", EnvContainerBin, "/usr/bin/podman")
		if got := engineKind(); got != EngineDocker {
			t.Fatalf("engineKind() = %q, want docker (explicit env beats bin inference)", got)
		}
	})
	t.Run("inferred from a podman binary path", func(t *testing.T) {
		clearEngineEnv(t, EnvContainerBin, "/usr/local/bin/podman-remote")
		if got := engineKind(); got != EnginePodman {
			t.Fatalf("engineKind() = %q, want podman (inferred from bin basename)", got)
		}
	})
	t.Run("unrecognised value falls through to docker", func(t *testing.T) {
		clearEngineEnv(t, EnvRuntimeEngine, "containerd")
		if got := engineKind(); got != EngineDocker {
			t.Fatalf("engineKind() = %q, want docker (typo must not select the other engine)", got)
		}
	})
}

func TestEngineBinary(t *testing.T) {
	t.Run("default is the engine name", func(t *testing.T) {
		clearEngineEnv(t)
		if got := EngineBinary(); got != "docker" {
			t.Fatalf("EngineBinary() = %q, want docker", got)
		}
		clearEngineEnv(t, EnvRuntimeEngine, "podman")
		if got := EngineBinary(); got != "podman" {
			t.Fatalf("EngineBinary() = %q, want podman", got)
		}
	})
	t.Run("explicit binary wins verbatim", func(t *testing.T) {
		clearEngineEnv(t, EnvRuntimeEngine, "podman", EnvContainerBin, "/opt/podman/bin/podman")
		if got := EngineBinary(); got != "/opt/podman/bin/podman" {
			t.Fatalf("EngineBinary() = %q, want the explicit path", got)
		}
	})
}

func TestPlaneHostAliasPerEngine(t *testing.T) {
	t.Run("docker", func(t *testing.T) {
		clearEngineEnv(t, EnvRuntimeEngine, "docker")
		if got := planeHostAlias(); got != "host.docker.internal" {
			t.Fatalf("alias = %q, want host.docker.internal", got)
		}
	})
	t.Run("podman", func(t *testing.T) {
		// Podman injects this name into every container; the Docker name does
		// not resolve there (and vice versa across a podman machine VM).
		clearEngineEnv(t, EnvRuntimeEngine, "podman")
		if got := planeHostAlias(); got != "host.containers.internal" {
			t.Fatalf("alias = %q, want host.containers.internal", got)
		}
	})
	t.Run("operator override beats both", func(t *testing.T) {
		for _, eng := range []string{"docker", "podman"} {
			clearEngineEnv(t, EnvRuntimeEngine, eng, EnvPlaneHostAlias, "plane-host.internal")
			if got := planeHostAlias(); got != "plane-host.internal" {
				t.Fatalf("%s: alias = %q, want the ORCHICON_PLANE_HOST_ALIAS override", eng, got)
			}
		}
	})
}

func TestHostGatewayArgs(t *testing.T) {
	t.Run("docker maps the alias to the host gateway", func(t *testing.T) {
		clearEngineEnv(t, EnvRuntimeEngine, "docker")
		args := hostGatewayArgs()
		if len(args) != 2 || args[0] != "--add-host" || args[1] != "host.docker.internal:host-gateway" {
			t.Fatalf("hostGatewayArgs() = %v, want [--add-host host.docker.internal:host-gateway]", args)
		}
	})
	t.Run("podman adds nothing (alias is injected)", func(t *testing.T) {
		clearEngineEnv(t, EnvRuntimeEngine, "podman")
		if args := hostGatewayArgs(); len(args) != 0 {
			t.Fatalf("hostGatewayArgs() = %v, want none — podman injects host.containers.internal and older builds reject host-gateway", args)
		}
	})
	t.Run("docker honours an alias override", func(t *testing.T) {
		clearEngineEnv(t, EnvRuntimeEngine, "docker", EnvPlaneHostAlias, "plane-host.internal")
		args := hostGatewayArgs()
		if len(args) != 2 || !strings.HasSuffix(args[1], "plane-host.internal:host-gateway") {
			t.Fatalf("hostGatewayArgs() = %v, want the overridden alias mapped to host-gateway", args)
		}
	})
}

func TestContainerResidencyNeedsOverride(t *testing.T) {
	clearEngineEnv(t, EnvRuntimeEngine, "docker")
	if containerResidencyNeedsOverride() {
		t.Fatal("docker container residency must keep its historical gateway guard (no override required)")
	}
	clearEngineEnv(t, EnvRuntimeEngine, "podman")
	if !containerResidencyNeedsOverride() {
		t.Fatal("podman container residency has no derivable bridge IP — an explicit ORCHICON_PLANE_PUBLIC_URL must be required")
	}
}
