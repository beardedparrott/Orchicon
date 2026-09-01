package mcpclient

import (
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// headerRoundTripper injects static request headers (e.g. Authorization)
// into every outbound HTTP request. The go-sdk has no per-transport
// header option, so header/bearer auth is plumbed through the http.Client
// (ADR-0008: full OAuth dance deferred; header auth in v1).
type headerRoundTripper struct {
	base    http.RoundTripper
	headers map[string]string
}

func (h *headerRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	r2 := r.Clone(r.Context())
	for k, v := range h.headers {
		r2.Header.Set(k, v)
	}
	return h.base.RoundTrip(r2)
}

// newHTTPTransport builds the go-sdk StreamableClientTransport for a
// remote streamable-HTTP MCP server. Header auth is applied via a custom
// http.Client; the standalone-SSE fallback is disabled (the go-sdk only
// enables SSE for compatibility with servers that do not implement the
// modern streamable HTTP protocol — we always send
// Accept: application/json, text/event-stream and disable the legacy
// mode so a server that only speaks SSE surfaces an error instead of a
// hang).
func newHTTPTransport(spec ServerSpec) (*mcp.StreamableClientTransport, error) {
	if spec.URL == "" {
		return nil, fmt.Errorf("mcp server %q: http transport requires a URL", spec.ID)
	}
	u, err := url.Parse(spec.URL)
	if err != nil {
		return nil, fmt.Errorf("mcp server %q: invalid url %q: %w", spec.ID, spec.URL, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("mcp server %q: url %q must be http(s)", spec.ID, spec.URL)
	}
	base := http.DefaultTransport
	var rt http.RoundTripper = base
	if len(spec.Headers) > 0 {
		rt = &headerRoundTripper{base: base, headers: spec.Headers}
	}
	return &mcp.StreamableClientTransport{
		Endpoint:             spec.URL,
		HTTPClient:           &http.Client{Transport: rt},
		MaxRetries:           1,
		DisableStandaloneSSE: true,
	}, nil
}

// connectTimeout returns the context timeout for a lazy connect+discover.
func connectTimeout(spec ServerSpec) time.Duration {
	if spec.Timeout > 0 {
		return spec.Timeout
	}
	return defaultConnectTimeout
}

// toolTimeout returns the per-call timeout for a tool on this server.
func toolTimeout(spec ServerSpec) time.Duration {
	if spec.Timeout > 0 {
		return spec.Timeout
	}
	return defaultToolCallTimeout
}
