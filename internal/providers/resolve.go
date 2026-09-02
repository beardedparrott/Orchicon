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
	u.Host = gwHost
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

// repairCandidates lists the most-likely-working base URLs for a custom
// provider whose stored URL cannot be probed: plane-reachable host
// (loopback → gateway, container mode only) and/or the missing version
// root (/v1 — the OpenAI-compat convention the custom wire appends
// /models and /chat/completions to). Candidates are ordered most-likely
// first and deduped; the ORIGINAL url is NOT included (the caller already
// tried it). Empty/nil when nothing can be sensibly tried.
func repairCandidates(raw string) []string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" || u.Port() == "" {
		return nil
	}
	hosts := []string{u.Host}
	if gw := hostGatewayIP(); gw != "" {
		switch u.Hostname() {
		case "localhost", "127.0.0.1", "0.0.0.0", "::1":
			hosts = append(hosts, net.JoinHostPort(gw, u.Port()))
		}
	}
	pathNeedsV1 := u.Path == "" || u.Path == "/"
	seen := map[string]bool{}
	var out []string
	for _, h := range hosts {
		c := *u
		c.Host = h
		if pathNeedsV1 {
			v := c
			v.Path = strings.TrimRight(c.Path, "/") + "/v1"
			if !seen[v.String()] {
				seen[v.String()] = true
				out = append(out, v.String())
			}
		}
		if !seen[c.String()] {
			seen[c.String()] = true
			out = append(out, c.String())
		}
	}
	return out
}