// STDLIB ONLY, for the same reason internal/planecontrol is: the desktop shell is a leaf client and
// must be able to use this without pulling the plane's HTTP stack (or a webview) in behind it.
package controlport

import (
	"net/url"
	"strconv"
)

// Defaults returns the control port per instance.
func Defaults() map[string]int {
	return map[string]int{"dev": 8080, "prod": 8091, "test": 8092}
}

// InstanceForPort resolves a control port to an instance.
func InstanceForPort(port int) (string, bool) {
	for instance, p := range Defaults() {
		if p == port {
			return instance, true
		}
	}
	return "", false
}

// InstanceForURL resolves http(s)://host:port to an instance.
func InstanceForURL(rawURL string) (string, bool) {
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		return "", false
	}
	port := 0
	if p := u.Port(); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil {
			return "", false
		}
		port = n
	}
	if port == 0 {
		switch u.Scheme {
		case "https":
			port = 443
		case "http":
			port = 80
		default:
			return "", false
		}
	}
	return InstanceForPort(port)
}

// HostPort returns the host:port a URL points at, for display in the window title.
func HostPort(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		return rawURL
	}
	return u.Host
}
