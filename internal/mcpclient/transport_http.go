package mcpclient

import (
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// headerRoundTripper injects static request headers (e.g. Authorization) into every outbound HTTP
// request.
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

// newHTTPTransport builds the go-sdk StreamableClientTransport for a remote streamable-HTTP MCP
// server.
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

// connectTimeoutEnv overrides defaultConnectTimeout for a deployment whose servers need longer to start
// (a Python server on a slow or cold filesystem is the measured case).
const connectTimeoutEnv = "ORCHICON_MCP_CONNECT_TIMEOUT"

// connectTimeout returns the context timeout for a lazy connect+discover.
func connectTimeout(spec ServerSpec) time.Duration {
	if spec.Timeout > 0 {
		return spec.Timeout
	}
	if d := envConnectTimeout(); d > 0 {
		return d
	}
	return defaultConnectTimeout
}

// envConnectTimeout reads connectTimeoutEnv, once per call. An unparseable value is ignored rather than
// fatal: a typo in an optional tuning knob must not stop a plane from starting servers at all.
func envConnectTimeout() time.Duration {
	raw := strings.TrimSpace(os.Getenv(connectTimeoutEnv))
	if raw == "" {
		return 0
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		return 0
	}
	return d
}

// toolTimeout returns the per-call timeout for a tool on this server.
func toolTimeout(spec ServerSpec) time.Duration {
	if spec.Timeout > 0 {
		return spec.Timeout
	}
	return defaultToolCallTimeout
}
