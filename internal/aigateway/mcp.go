package aigateway

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/user"
	"strings"
	"sync"
	"time"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
)

// MCPDiscoverer discovers MCP servers configured in opencode by shelling
// out to `opencode debug config`. Caches results with a TTL.
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

// resolvedConfig is the shape of the JSON returned by `opencode debug config`.
type resolvedConfig struct {
	MCP map[string]mcpEntry `json:"mcp"`
}

type mcpEntry struct {
	Type    string   `json:"type"`
	URL     string   `json:"url,omitempty"`
	Command any      `json:"command,omitempty"`
	Enabled *bool    `json:"enabled,omitempty"`
}

// fetchMCPs shells out to `opencode debug config` and reads the resolved
// config JSON, which includes the full (merged) MCP server list.
func (d *MCPDiscoverer) fetchMCPs(ctx context.Context) ([]*apiv1.OpenCodeMCP, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, d.binary, "debug", "config")
	cmd.Dir = homeDir()
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("opencode debug config: %w", err)
	}

	var cfg resolvedConfig
	if err := json.Unmarshal(out, &cfg); err != nil {
		return nil, fmt.Errorf("parse opencode config: %w", err)
	}

	var servers []*apiv1.OpenCodeMCP
	for name, entry := range cfg.MCP {
		enabled := true
		if entry.Enabled != nil {
			enabled = *entry.Enabled
		}
		if !enabled {
			continue
		}

		cmdStr := ""
		switch v := entry.Command.(type) {
		case string:
			cmdStr = v
		case []any:
			parts := make([]string, 0, len(v))
			for _, p := range v {
				if s, ok := p.(string); ok {
					parts = append(parts, s)
				}
			}
			cmdStr = strings.Join(parts, " ")
		}
		if cmdStr == "" {
			cmdStr = entry.URL
		}

		servers = append(servers, &apiv1.OpenCodeMCP{
			Id:      name,
			Command: cmdStr,
			Status:  "configured",
		})
	}

	servers = mergeMCPs(servers, wellKnownMCPs)
	return servers, nil
}

// homeDir returns the user's home directory.
func homeDir() string {
	if u, err := user.Current(); err == nil {
		return u.HomeDir
	}
	return os.Getenv("HOME")
}

// wellKnownMCPs is a curated list of common MCP servers available for
// workers. These are merged with the user's configured servers so they
// are always selectable in the MCP picker regardless of local config.
var wellKnownMCPs = []*apiv1.OpenCodeMCP{
	{Id: "context7", Command: "https://mcp.context7.com/mcp", Status: "available"},
	{Id: "gh_grep", Command: "https://mcp.grep.app", Status: "available"},
	{Id: "filesystem", Command: "npx -y @modelcontextprotocol/server-filesystem", Status: "available"},
	{Id: "github", Command: "npx -y @modelcontextprotocol/server-github", Status: "available"},
	{Id: "postgres", Command: "npx -y @modelcontextprotocol/server-postgres", Status: "available"},
	{Id: "memory", Command: "npx -y @modelcontextprotocol/server-memory", Status: "available"},
	{Id: "playwright", Command: "npx -y @playwright/mcp", Status: "available"},
	{Id: "slack", Command: "npx -y @modelcontextprotocol/server-slack", Status: "available"},
}

// mergeMCPs merges configured servers with well-known ones.
// Configured servers take precedence (same id → configured wins).
func mergeMCPs(configured, wellknown []*apiv1.OpenCodeMCP) []*apiv1.OpenCodeMCP {
	seen := make(map[string]bool, len(configured))
	result := make([]*apiv1.OpenCodeMCP, 0, len(configured)+len(wellknown))
	result = append(result, configured...)
	for _, s := range configured {
		seen[s.Id] = true
	}
	for _, s := range wellknown {
		if !seen[s.Id] {
			result = append(result, s)
			seen[s.Id] = true
		}
	}
	return result
}

// MockMCPDiscoverer returns a discoverer with a hardcoded server list.
func MockMCPDiscoverer(log *slog.Logger) *MCPDiscoverer {
	d := &MCPDiscoverer{
		log:    log.With("component", "mcp_discoverer"),
		binary: "",
		ttl:    24 * time.Hour,
	}
	d.cache = mergeMCPs(nil, wellKnownMCPs)
	d.cached = time.Now()
	return d
}
