package providers

import (
	"net"
	"net/url"
	"os"
	"strings"
)

// Container mode ONLY (ORCHICON_CONTAINER_MODE=1): on a host-run plane there is no bridge and
// localhost already reaches the host — rewriting anything there would break correct configurations.
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

// Container mode ONLY (see hostGatewayIP).
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
	// PRESERVE THE PORT: u.Host is the combined host:port field — replacing it with the bare gateway IP
	// silently dropped ":8095" (QA round 4: stored URLs lost their port, probe hit port 80.
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

// TransposeForContainer rewrites a provider base URL whose host is the HOST's loopback into the
// address a runtime container can dial.
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
		// A portless loopback URL cannot be transposed usefully — the address is not the problem, the
		// missing port is, and guessing one would point the worker at a service that is not the model.
		return trimmed
	}
	u.Host = net.JoinHostPort(ContainerHostAddress, u.Port())
	return u.String()
}

// mustTryLocalPorts reports whether a host is one where the common local-inference ports are worth
// trying.
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

// repairCandidates lists the most-likely-working base URLs for a custom provider whose stored URL
// cannot be probed.
func repairCandidates(raw string) []string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return nil
	}

	gw := hostGatewayIP()

	// Hosts to try, in order: the stored host itself, then the gateway (when the stored host is
	// loopback OR is a portless gateway artifact, both being container-reachability failures).
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
	pathNeedsV1 := u.Path == "" || u.Path == "/"

	port := u.Port()
	ports := []string{port}
	if port == "" {
		switch {
		case mustTryLocalPorts(u.Hostname()):
			ports = []string{"8080", "8095", "8000", "11434"}
		case !pathNeedsV1:
			// A PUBLIC host, portless, whose path ALREADY carries the version root.
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
