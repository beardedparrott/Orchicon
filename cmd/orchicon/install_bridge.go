package main

// install_bridge.go — the DOCKER BRIDGE address, resolved only when runtime containers are in use.
//
// WHY THIS IS NOT IN install_host.go ANY MORE. install_host.go is the host-residency install path, and a
// `docker` invocation sitting in it is exactly the kind of thing that fails silently on a machine with no
// Docker. The bridge lookup is not dead code — it still answers "which address do this instance's runtime
// containers use to reach the HOST plane" — so it lives here, in the one file whose caller is allowed to
// need it: planeBindEnv, which does not even call bridgeIP unless runtimeContainersInUse() is true.

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
)

// bridgeIP resolves the docker bridge address — the address a runtime container
// on the default bridge uses to reach the HOST.
//
// A HOST-RESIDENT plane must bind it, and a published host port cannot be used
// in its place: docker's hairpin NAT drops gateway→published-port traffic from
// bridge containers, which is exactly the plane-channel MCP timeout this bind
// removes.
//
// Resolution order mirrors container.sh: ORCHICON_DOCKER_BRIDGE_IP (tests and
// hand-pinning) → docker's own IPAM config → the docker0 interface address.
//
// An unresolvable bridge is a HARD error, and it is only ever reached when
// runtime containers ARE in use (planeBindEnv checks first). There is
// deliberately no 0.0.0.0 fallback — that is the LAN exposure the operator
// rejected — and no guessed address, because a wrong bind surfaces much later as
// "workers cannot reach the plane" rather than as a failure here.
func bridgeIP() (string, error) {
	if v := strings.TrimSpace(os.Getenv("ORCHICON_DOCKER_BRIDGE_IP")); v != "" {
		return v, nil
	}
	if out, err := exec.Command("docker", "network", "inspect", "bridge",
		"--format", "{{(index .IPAM.Config 0).Gateway}}").Output(); err == nil {
		ip := strings.TrimSpace(string(out))
		// The Go template prints "<no value>" for a nil gateway.
		if ip != "" && ip != "<no value>" && ip != "null" {
			return ip, nil
		}
	}
	// docker0 fallback through net.Interfaces rather than shelling out to `ip`:
	// no external binary, and it works wherever the interface is readable.
	if iface, err := net.InterfaceByName("docker0"); err == nil {
		if addrs, err := iface.Addrs(); err == nil {
			for _, a := range addrs {
				if ipn, ok := a.(*net.IPNet); ok && ipn.IP.To4() != nil {
					return ipn.IP.String(), nil
				}
			}
		}
	}
	return "", fmt.Errorf("cannot resolve the docker bridge address (docker network inspect bridge / docker0) — " +
		"pin it with ORCHICON_DOCKER_BRIDGE_IP=<bridge gateway ip>; refusing to guess or bind 0.0.0.0")
}
