package runtime

// Instance scoping (daemon.go instanceLabelArgs / listRuntimes) and the
// per-engine host-gateway mapping as they appear in the create arg list.
//
// Why it matters: two instances (dev, prod) can share ONE daemon. A runtime
// container carries the instance label, and the reapers filter on it — so a
// missing or shared label value would let dev's sweep reap prod's runtimes
// mid-run. Both engines accept `--filter label=k=v` identically, so the
// filter syntax is not the risk; the label WRITE is.

import (
	"strings"
	"testing"
)

func TestInstanceLabelArgs(t *testing.T) {
	t.Run("dual-written under both keys", func(t *testing.T) {
		args := instanceLabelArgs("prod")
		joined := strings.Join(args, " ")
		for _, want := range []string{"orchicon.instance=prod", "io.orchicon.instance=prod"} {
			if !strings.Contains(joined, want) {
				t.Fatalf("instance labels %v do not contain %q", args, want)
			}
		}
	})

	t.Run("defaults to dev when the caller names no instance", func(t *testing.T) {
		joined := strings.Join(instanceLabelArgs(""), " ")
		if !strings.Contains(joined, "orchicon.instance=dev") || !strings.Contains(joined, "io.orchicon.instance=dev") {
			t.Fatalf("default instance labels = %q, want both keys with value dev", joined)
		}
	})

	t.Run("each label is a separate flag pair", func(t *testing.T) {
		args := instanceLabelArgs("prod")
		if len(args) != 4 {
			t.Fatalf("instanceLabelArgs = %v, want 4 elements (two --label/value pairs)", args)
		}
		for i := 0; i < len(args); i += 2 {
			if args[i] != "--label" {
				t.Fatalf("arg %d = %q, want --label (a bare value would be read as the image name)", i, args[i])
			}
		}
	})
}

// TestCreateArgsCarryInstanceLabelsAndAlias pins the assembling side: the
// container the daemon creates is scoped to the requesting instance and can
// resolve the plane host alias for the selected engine.
func TestCreateArgsCarryInstanceLabelsAndAlias(t *testing.T) {
	clearEngineEnv(t, EnvRuntimeEngine, "docker")

	args := []string{"--label", "orchicon.workflow=" + "wf"}
	args = append(args, instanceLabelArgs("prod")...)
	args = append(args, hostGatewayArgs()...)
	joined := strings.Join(args, " ")

	for _, want := range []string{
		"orchicon.instance=prod",
		"io.orchicon.instance=prod",
		"--add-host host.docker.internal:host-gateway",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("create args %q missing %q", joined, want)
		}
	}

	// Podman: the Docker name is not mapped (podman injects its own).
	clearEngineEnv(t, EnvRuntimeEngine, "podman")
	podmanArgs := append(instanceLabelArgs("prod"), hostGatewayArgs()...)
	if strings.Contains(strings.Join(podmanArgs, " "), "host.docker.internal") {
		t.Fatalf("podman create args must not map the docker-only host name: %v", podmanArgs)
	}
}
