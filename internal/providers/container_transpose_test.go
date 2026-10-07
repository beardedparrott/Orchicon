package providers

// container_transpose_test.go — a provider URL transposed for a RUNTIME CONTAINER.
//
// The operator: "we now have two different IP addresses to reference the same model.
// 127.0.0.1 on the local host, and whatever the IP inside the runtime container is…
// Previously the GUI under provider settings would automatically transpose the
// container IP when you put in 127.0.0.1 and saved a new provider. This doesn't seem
// to be happening anymore."
//
// The old rewrite was gated on ORCHICON_CONTAINER_MODE=1 — correct when the control
// plane ran in a container, dead now that it runs on the host. And the address it
// targeted (the bridge gateway) is not what a container needs either: the fix is to
// give the CONTAINER a transposed view while the stored row keeps what was typed,
// because the host-plane consumer needs the loopback that the container cannot use.
//
// MEASURED on this host, which is why the target is the bridge IP and not
// host.docker.internal:
//
//	                      on the HOST        inside a CONTAINER
//	127.0.0.1:<port>      works              the container's own loopback — nothing there
//	host.docker.internal  DOES NOT RESOLVE   resolves (--add-host)
//	172.17.0.1:<port>     works              works

import (
	"testing"
)

func TestTransposeForContainerRewritesLoopback(t *testing.T) {
	cases := map[string]string{
		// The operator's real rows, and the value the container actually needs.
		"http://127.0.0.1:8741/v1": "http://172.17.0.1:8741/v1",
		"http://127.0.0.1:8731/v1": "http://172.17.0.1:8731/v1",
		"http://localhost:8095/v1": "http://172.17.0.1:8095/v1",
		"http://0.0.0.0:8000":      "http://172.17.0.1:8000",
		"http://[::1]:11434/v1":    "http://172.17.0.1:11434/v1",
		// Not loopback: reachable as it stands, so untouched.
		"http://172.17.0.1:8095/v1":           "http://172.17.0.1:8095/v1",
		"https://api.deepseek.com":            "https://api.deepseek.com",
		"http://192.168.50.232:8741":          "http://192.168.50.232:8741",
		"http://host.docker.internal:8741/v1": "http://host.docker.internal:8741/v1",
		// Portless loopback: the missing PORT is the problem, not the address, and
		// guessing a port would point the worker at a different service.
		"http://127.0.0.1": "http://127.0.0.1",
		// Junk passes through rather than becoming a half-built URL.
		"not a url": "not a url",
		"":          "",
	}
	for in, want := range cases {
		if got := TransposeForContainer(in); got != want {
			t.Errorf("TransposeForContainer(%q) = %q, want %q", in, got, want)
		}
	}
}

// THE STORED URL IS NOT TOUCHED. The transposition is a VIEW for one consumer, not
// a write — that is the whole point of doing it here rather than in the provider
// row, and it is what keeps the host-plane consumer working.
func TestTransposingDoesNotMutateTheStoredValue(t *testing.T) {
	stored := "http://127.0.0.1:8741/v1"
	_ = TransposeForContainer(stored)
	if stored != "http://127.0.0.1:8741/v1" {
		t.Fatalf("the input was mutated: %q", stored)
	}
	// …and the two consumers genuinely differ, which is the fact the design rests on.
	if TransposeForContainer(stored) == stored {
		t.Fatal("a container view must differ from the host view for a loopback URL")
	}
}

// The target must be the address CONTAINERS can dial — asserted explicitly, because
// getting this wrong in EITHER direction is the bug being fixed: host.docker.internal
// does not resolve on this host (verified), and 127.0.0.1 does not reach the host
// from a container (verified).
func TestTheContainerTargetIsTheBridgeAddress(t *testing.T) {
	got := TransposeForContainer("http://127.0.0.1:8741/v1")
	if got != "http://172.17.0.1:8741/v1" {
		t.Fatalf("transposed to %q, want the docker0 bridge address — the only value "+
			"reachable from BOTH the host and a container on a native-Linux docker host", got)
	}
	if ContainerHostAddress != "172.17.0.1" {
		t.Fatalf("ContainerHostAddress = %q; the measured host reachability is 172.17.0.1 "+
			"(docker0), and host.docker.internal does NOT resolve on the host", ContainerHostAddress)
	}
}
