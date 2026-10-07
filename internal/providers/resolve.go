package providers

import (
	"net"
	"net/url"
	"os"
	"strings"
)

// hostGatewayIP returns the IP the container can use to reach services on
// the HOST (the docker bridge gateway), or "" when not applicable.
//
// Detection: connect a UDP socket toward a public address — the local
// address the kernel picks is the container's interface on the default
// bridge. The gateway (the host, from the container's perspective) is the
// first address in that subnet (the standard docker bridge 172.17.0.0/16 →
// 172.17.0.1). The UDP "connect" only picks a route; no packet is sent.
//
// Container mode ONLY (ORCHICON_CONTAINER_MODE=1): on a host-run plane
// there is no bridge and localhost already reaches the host — rewriting
// anything there would break correct configurations. Undetectable gateway
// (odd network setups) also returns "" — never guess.
func hostGatewayIP() string {
	if os.Getenv("ORCHICON_CONTAINER_MODE") != "1" {
		return ""
	}
	conn, err := net.Dial("udp", "8.8.8.8:53")
	if err != nil {
		return ""
	}
	defer conn.Close()
	la, ok := conn.LocalAddr().(*net.UDPAddr)
	if !ok || la.IP == nil {
		return ""
	}
	ipnet := net.IPNet{IP: la.IP, Mask: la.IP.DefaultMask()}
	base := ipnet.IP.To16()
	if base == nil {
		return ""
	}
	gw := make(net.IP, len(base))
	copy(gw, base)
	gw[len(gw)-1] = 1 // network address + 1 = conventional gateway
	return gw.String()
}

// resolveForPlane rewrites loopback addresses in a custom provider base URL
// to the container's host gateway (docker bridge) so a user-entered
// "localhost:8095" reaches their host machine. Container mode ONLY (see
// hostGatewayIP).
//
// Returns (resolvedURL, note, changed). note is operator-facing plain
// language (no docker jargon) explaining what was rewritten and why, or ""
// when nothing changed.
func resolveForPlane(raw string) (string, string, bool) {
	trimmed := strings.TrimSpace(raw)
	u, err := url.Parse(trimmed)
	if err != nil || u.Hostname() == "" || u.Port() == "" {
		return trimmed, "", false
	}
	host := u.Hostname()
	if host != "localhost" && host != "127.0.0.1" && host != "0.0.0.0" && host != "::1" {
		return trimmed, "", false
	}
	gw := hostGatewayIP()
	if gw == "" {
		return trimmed, "", false
	}
	gwHost := gw
	if strings.Contains(gw, ":") {
		gwHost = "[" + gw + "]"
	}
	// PRESERVE THE PORT: u.Host is the combined host:port field — replacing
	// it with the bare gateway IP silently dropped ":8095" (QA round 4:
	// stored URLs lost their port, probe hit port 80, and repairCandidates
	// bailed on the portless URL — the resolver poisoned its own repair
	// path). JoinHostPort keeps host AND port.
	u.Host = net.JoinHostPort(gwHost, u.Port())
	resolved := u.String()
	if resolved == trimmed {
		return trimmed, "", false
	}
	note := "Rewrote " + host + " to " + gw + " automatically: this app runs in a container, " +
		"and from inside it 'localhost' means the app itself — not your computer. " +
		gw + " is the address this app can use to reach services on your machine. " +
		"If models still don't appear: make sure your server listens on all interfaces " +
		"(llama-server: --host 0.0.0.0) and that the URL includes the version root (ends in /v1)."
	return resolved, note, true
}

// ContainerHostAddress is the address a RUNTIME CONTAINER uses to reach a
// service listening on the HOST's loopback.
//
// MEASURED, not assumed, because the two Docker flavours differ:
//
//	                       on the HOST        inside a CONTAINER
//	127.0.0.1:<port>       works              the container's OWN loopback — nothing there
//	host.docker.internal   DOES NOT RESOLVE   resolves via --add-host (daemon.go)
//	172.17.0.1:<port>      works (docker0)    works
//
// So `host.docker.internal` is a CONTAINER-ONLY name: rewriting a stored host
// value to it would break the host-plane consumer that works today (verified on
// this host — `getent hosts host.docker.internal` returns nothing outside a
// container). The bridge IP is the one value that works from BOTH sides, so it
// is what a transposition uses. It is still not universally right — a
// non-default bridge subnet moves it — which is why the operator is warned
// about the firewall rule and the address is stated in the UI.
const ContainerHostAddress = "172.17.0.1"

// TransposeForContainer rewrites a provider base URL whose host is the HOST's
// loopback into the address a runtime container can dial.
//
// A runtime container's loopback is its own, so a local model published on the
// operator's machine is unreachable from a worker at 127.0.0.1 — while the same
// URL is exactly right for the host-plane consumer that dials it in-process.
// One stored provider therefore needs two addresses, and this is the container's.
//
// 0.0.0.0 and ::1 are rewritten too, for the same reason: none of them name the
// host from inside a container.
//
// The PORT and PATH are preserved (net.JoinHostPort), because a local model's
// port IS its identity and the version root (…/v1) is part of the endpoint.
// A URL that is not loopback is returned unchanged: a public endpoint, a LAN
// address and an already-transposed value are all reachable as they stand.
func TransposeForContainer(raw string) string {
	trimmed := strings.TrimSpace(raw)
	u, err := url.Parse(trimmed)
	if err != nil || u.Hostname() == "" {
		return trimmed
	}
	switch u.Hostname() {
	case "localhost", "127.0.0.1", "0.0.0.0", "::1":
	default:
		return trimmed
	}
	if u.Port() == "" {
		// A portless loopback URL cannot be transposed usefully — the address is
		// not the problem, the missing port is, and guessing one would point the
		// worker at a service that is not the model.
		return trimmed
	}
	u.Host = net.JoinHostPort(ContainerHostAddress, u.Port())
	return u.String()
}

// mustTryLocalPorts reports whether a host is one where the common
// local-inference ports are worth trying.
//
// TRUE only for an endpoint reachable on this machine or this network: loopback,
// the container's host gateway, a private range, or a bare hostname with no dots
// (a docker service name like `ollama`). FALSE for a public host — see the port
// fallback in repairCandidates for the cost of getting this wrong.
func mustTryLocalPorts(host string) bool {
	h := strings.TrimSpace(host)
	if h == "" {
		return true // a portless parse: keep the historical behaviour
	}
	switch h {
	case "localhost", "0.0.0.0", "::1", "host.docker.internal", "gateway.docker.internal":
		return true
	}
	// A bare service name (docker compose network) is local by construction.
	if !strings.Contains(h, ".") && !strings.Contains(h, ":") {
		return true
	}
	if ip := net.ParseIP(h); ip != nil {
		return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast()
	}
	return false
}

// repairCandidates lists the most-likely-working base URLs for a custom
// provider whose stored URL cannot be probed. Dimensions explored:
//   - host: loopback → plane-reachable host gateway (container mode only;
//     the portless-URL legacy bug may also have stored a bare gateway),
//   - port: preserved as entered; portless URLs (legacy bug) try common
//     llama-server/vLLM-class ports,
//   - path: the version root (/v1) appended when absent.
//
// Candidates are ordered most-likely first and deduped; the ORIGINAL URL
// is NOT included (the caller already tried it). Empty/nil when nothing
// can be sensibly tried.
func repairCandidates(raw string) []string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return nil
	}

	gw := hostGatewayIP()

	// Hosts to try, in order: the stored host itself, then the gateway
	// (when the stored host is loopback OR is a portless gateway artifact,
	// both being container-reachability failures).
	hosts := []string{u.Host}
	loopbackOrGateway := false
	switch u.Hostname() {
	case "localhost", "127.0.0.1", "0.0.0.0", "::1":
		loopbackOrGateway = true
	case "", gw: // portless parse or already-stored-gateway artifact
		loopbackOrGateway = true
	}
	if loopbackOrGateway && gw != "" {
		hosts = append(hosts, gw)
	}

	// PORT FALLBACK, LOCAL ENDPOINTS ONLY.
	//
	// The common local-inference ports belong to an endpoint on THIS network
	// (ollama 11434, vLLM 8000, a dev proxy on 8080/8095). Applying them to a
	// PUBLIC host manufactures addresses that cannot exist: repairing anthropic's
	// `https://api.anthropic.com/v1` produced api.anthropic.com:11434 and friends,
	// nine probes of which several hung for 15-30s on the dial — 45 SECONDS IN
	// TOTAL, which is exactly the model picker's budget, so the picker never got
	// an answer and the operator saw a list stuck on "loading models…".
	//
	// A public host is repaired by a missing version root or nothing at all; it
	// does not move to another port. `for a custom` in this function's doc is the
	// intent: a locally-entered endpoint, not a built-in cloud provider.
	pathNeedsV1 := u.Path == "" || u.Path == "/"

	port := u.Port()
	ports := []string{port}
	if port == "" {
		switch {
		case mustTryLocalPorts(u.Hostname()):
			ports = []string{"8080", "8095", "8000", "11434"}
		case !pathNeedsV1:
			// A PUBLIC host, portless, whose path ALREADY carries the version root.
			// The URL is well formed and there is nothing to repair: a 401 from it is
			// a TOKEN problem, not an endpoint one. Returning here is what keeps a
			// built-in cloud provider out of the sweep entirely.
			//
			// This is the case that cost 45 seconds: `https://api.anthropic.com/v1`
			// reached the port fallback, which invented api.anthropic.com:11434 and
			// friends, each dial blocking on a timeout.
			return nil
		}
	}

	seen := map[string]bool{}
	var out []string
	add := func(host, port, path string) {
		c := *u
		if port == "" {
			// JoinHostPort would render "host:" — a malformed URL. A portless
			// candidate keeps the host verbatim.
			c.Host = host
		} else {
			c.Host = net.JoinHostPort(host, port)
		}
		c.Path = path
		s := c.String()
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	for _, h := range hosts {
		for _, p := range ports {
			if pathNeedsV1 {
				add(h, p, strings.TrimRight(u.Path, "/")+"/v1")
			}
			add(h, p, u.Path)
		}
	}
	// Drop the original URL (the caller already tried it).
	if orig := u.String(); len(out) > 0 && out[0] == orig {
		out = out[1:]
	} else {
		for i, s := range out {
			if s == orig {
				out = append(out[:i], out[i+1:]...)
				break
			}
		}
	}
	return out
}
