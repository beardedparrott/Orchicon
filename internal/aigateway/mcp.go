package aigateway

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
)

// MCPDiscoverer discovers MCP servers configured in opencode by shelling
// out to `opencode mcp list`. Caches results with a TTL.
type MCPDiscoverer struct {
	log    *slog.Logger
	binary string

	mu     sync.RWMutex
	cache  []*apiv1.OpenCodeMCP
	cached time.Time
	ttl    time.Duration
}

func NewMCPDiscoverer(log *slog.Logger, binary string) *MCPDiscoverer {
	if binary == "" {
		binary = "opencode"
	}
	return &MCPDiscoverer{
		log:    log.With("component", "mcp_discoverer"),
		binary: binary,
		ttl:    5 * time.Minute,
	}
}

// ListMCPs returns all MCP servers from opencode, cached.
func (d *MCPDiscoverer) ListMCPs(ctx context.Context) ([]*apiv1.OpenCodeMCP, error) {
	d.mu.RLock()
	if d.cache != nil && time.Since(d.cached) < d.ttl {
		d.mu.RUnlock()
		return d.cache, nil
	}
	d.mu.RUnlock()

	d.mu.Lock()
	defer d.mu.Unlock()

	if d.cache != nil && time.Since(d.cached) < d.ttl {
		return d.cache, nil
	}

	servers, err := d.fetchMCPs(ctx)
	if err != nil {
		if d.cache != nil {
			d.log.Warn("failed to refresh MCP servers from opencode, using stale cache", "error", err)
			return d.cache, nil
		}
		return nil, err
	}
	d.cache = servers
	d.cached = time.Now()
	return servers, nil
}

// fetchMCPs shells out to `opencode mcp list` and parses the output.
//
// The output format (ANSI-stripped) is:
//
//	┌  MCP Servers
//	│
//	●  ✓ godot connected
//	│      npx -y @coding-solo/godot-mcp
//	│
//	●  ✓ context7 connected
//	│      https://mcp.context7.com/mcp
//	│
//	└  3 server(s)
func (d *MCPDiscoverer) fetchMCPs(ctx context.Context) ([]*apiv1.OpenCodeMCP, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, d.binary, "mcp", "list")
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("opencode mcp list: %w", err)
	}
	return parseMCPListOutput(out)
}

// serverLine matches the MCP server status line.
// Example: "●  ✓ godot connected"
var serverLine = regexp.MustCompile(`●\s+[✓✗]\s+(\S+)\s+(\S+)`)

func parseMCPListOutput(data []byte) ([]*apiv1.OpenCodeMCP, error) {
	scanner := bufio.NewScanner(bytes.NewReader(data))
	var servers []*apiv1.OpenCodeMCP
	var current *apiv1.OpenCodeMCP
	expectCommand := false

	for scanner.Scan() {
		raw := scanner.Text()
		line := stripANSI(raw)

		if expectCommand {
			// The command line follows with indentation: "│      <command>"
			trimmed := strings.TrimSpace(line)
			trimmed = strings.TrimPrefix(trimmed, "│")
			trimmed = strings.TrimSpace(trimmed)
			if trimmed != "" && current != nil {
				current.Command = trimmed
			}
			expectCommand = false
			continue
		}

		if matches := serverLine.FindStringSubmatch(line); len(matches) >= 3 {
			current = &apiv1.OpenCodeMCP{
				Id:     matches[1],
				Status: matches[2],
			}
			servers = append(servers, current)
			expectCommand = true
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan MCP list output: %w", err)
	}

	return servers, nil
}

// stripANSI removes ANSI escape sequences from a string.
func stripANSI(s string) string {
	var buf bytes.Buffer
	in := false
	for _, ch := range s {
		if ch == '\x1b' {
			in = true
			continue
		}
		if in {
			if ch == 'm' || (ch >= 'A' && ch <= 'Z') || (ch >= 'a' && ch <= 'z') {
				in = false
			}
			continue
		}
		buf.WriteRune(ch)
	}
	return buf.String()
}

// MockMCPDiscoverer returns a discoverer with a hardcoded server list.
func MockMCPDiscoverer(log *slog.Logger) *MCPDiscoverer {
	d := &MCPDiscoverer{
		log: log.With("component", "mcp_discoverer"),
		binary: "",
		ttl: 24 * time.Hour,
	}
	d.cache = []*apiv1.OpenCodeMCP{
		{Id: "filesystem", Command: "npx -y @modelcontextprotocol/server-filesystem", Status: "connected"},
		{Id: "github", Command: "npx -y @modelcontextprotocol/server-github", Status: "connected"},
		{Id: "postgres", Command: "npx -y @modelcontextprotocol/server-postgres", Status: "connected"},
	}
	d.cached = time.Now()
	return d
}
