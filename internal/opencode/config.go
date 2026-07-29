package opencode

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// opencodeConfigPath returns the path to the opencode config file
// (opencode.json or opencode.jsonc) by checking, in order:
//  1. OPENCODE_CONFIG_DIR/<file>
//  2. $XDG_CONFIG_HOME/opencode/<file>
//  3. ~/.config/opencode/<file>
func openCodeConfigPath() string {
	dir := os.Getenv("OPENCODE_CONFIG_DIR")
	if dir != "" {
		if p := filepath.Join(dir, "opencode.json"); fileExists(p) {
			return p
		}
		if p := filepath.Join(dir, "opencode.jsonc"); fileExists(p) {
			return p
		}
	}

	xdg := os.Getenv("XDG_CONFIG_HOME")
	if xdg != "" {
		dir = filepath.Join(xdg, "opencode")
		if p := filepath.Join(dir, "opencode.json"); fileExists(p) {
			return p
		}
		if p := filepath.Join(dir, "opencode.jsonc"); fileExists(p) {
			return p
		}
	}

	home, _ := os.UserHomeDir()
	if home != "" {
		dir = filepath.Join(home, ".config", "opencode")
		if p := filepath.Join(dir, "opencode.json"); fileExists(p) {
			return p
		}
		if p := filepath.Join(dir, "opencode.jsonc"); fileExists(p) {
			return p
		}
	}

	return ""
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// readOpenCodeConfig reads the user's opencode config file and returns
// the raw map. Returns nil if the file cannot be read. If log is nil,
// warnings are suppressed.
func readOpenCodeConfig(log *slog.Logger) map[string]any {
	if log == nil {
		log = slog.Default()
	}
	path := openCodeConfigPath()
	if path == "" {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		log.Warn("opencode: cannot read config file", "path", path, "error", err)
		return nil
	}
	var cfg map[string]any
	if err := json.Unmarshal(data, &cfg); err != nil {
		log.Warn("opencode: cannot parse config file", "path", path, "error", err)
		return nil
	}
	return cfg
}

// readMCPServers extracts the MCP server definitions from the user's
// opencode config. It checks both the `mcp` key (opencode schema) and
// `mcpServers` key (MCP standard) for compatibility.
func readMCPServers(log *slog.Logger) map[string]any {
	cfg := readOpenCodeConfig(log)
	if cfg == nil {
		return nil
	}

	// Prefer the `mcp` key (opencode schema).
	if mcp, ok := cfg["mcp"]; ok {
		if mcpMap, ok := mcp.(map[string]any); ok {
			return normalizeMCPEntries(mcpMap)
		}
	}

	// Fall back to `mcpServers` (MCP standard notation).
	if mcpServers, ok := cfg["mcpServers"]; ok {
		if mcpServersMap, ok := mcpServers.(map[string]any); ok {
			return normalizeMCPEntries(mcpServersMap)
		}
	}

	return nil
}

// normalizeMCPEntries ensures every MCP entry has proper structure.
// opencode expects the `command` field as an array of strings, but
// some configs may store it as a single string — normalize it.
func normalizeMCPEntries(entries map[string]any) map[string]any {
	result := make(map[string]any, len(entries))
	for name, entry := range entries {
		e, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		// Normalize command: string → []string.
		if cmd, ok := e["command"]; ok {
			switch v := cmd.(type) {
			case string:
				parts := strings.Fields(v)
				e["command"] = parts
			case []any:
				parts := make([]string, 0, len(v))
				for _, p := range v {
					if s, ok := p.(string); ok {
						parts = append(parts, s)
					}
				}
				e["command"] = parts
			case []string:
				// Already in the right format.
			}
		}
		// Remove fields that are not part of the opencode config schema.
		for _, k := range []string{"env", "cwd", "timeout", "enabled"} {
			if _, ok := e[k]; !ok {
				continue
			}
			// Keep enabled, env, cwd, timeout — they're valid opencode mcp fields.
			_ = k
		}
		result[name] = e
	}
	return result
}

// BuildConfigContent builds the JSON string for the OPENCODE_CONFIG_CONTENT
// env var. It merges the agent configuration with MCP servers from the
// user's opencode config file, so worker executions automatically inherit
// the user's MCP tools.
//
// agentName is the name of the agent in the config (e.g. "orchicon-worker").
// agentPrompt is the system prompt text for the agent.
// modelRef is the model reference string.
// If agentPrompt is empty, no custom agent is added (opencode uses its default).
func BuildConfigContent(agentName, agentPrompt, modelRef string) string {
	cfg := map[string]any{
		"$schema": "https://opencode.ai/config.json",
	}

	if agentPrompt != "" {
		cfg["agent"] = map[string]any{
			agentName: map[string]any{
				"prompt": agentPrompt,
				"mode":   "primary",
				"model":  modelRef,
			},
		}
	}

	// Merge MCP servers from the user's opencode config.
	// We use a package-level logger since config is read at startup time.
	if mcpServers := readMCPServers(slog.Default()); len(mcpServers) > 0 {
		cfg["mcp"] = mcpServers
	}

	b, err := json.Marshal(cfg)
	if err != nil {
		slog.Default().Warn("opencode: marshal config content", "error", err)
		// Fall back to minimal config.
		fallback := map[string]any{
			"$schema": "https://opencode.ai/config.json",
		}
		if agentPrompt != "" {
			fallback["agent"] = map[string]any{
				agentName: map[string]any{
					"prompt": agentPrompt,
					"mode":   "primary",
					"model":  modelRef,
				},
			}
		}
		b, _ = json.Marshal(fallback)
	}
	return string(b)
}

// ReadMCPServersFromConfig reads the user's opencode config and returns
// the MCP server map. Returns nil if no config or no MCP servers found.
func ReadMCPServersFromConfig(log *slog.Logger) map[string]any {
	return readMCPServers(log)
}

// SanitizeMCPForSubprocess strips sensitive fields from MCP entries
// before passing them to a subprocess. Keeps only fields recognized by
// the opencode config schema (type, command, url, enabled, timeout, cwd).
func SanitizeMCPForSubprocess(entries map[string]any) map[string]any {
	result := make(map[string]any, len(entries))
	for name, entry := range entries {
		e, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		cleaned := make(map[string]any)
		for _, k := range []string{"type", "command", "url", "enabled", "timeout", "cwd"} {
			if v, ok := e[k]; ok {
				cleaned[k] = v
			}
		}
		// Normalize command to array.
		if cmd, ok := cleaned["command"]; ok {
			switch v := cmd.(type) {
			case string:
				cleaned["command"] = strings.Fields(v)
			case []any:
				parts := make([]string, 0, len(v))
				for _, p := range v {
					if s, ok := p.(string); ok {
						parts = append(parts, s)
					}
				}
				cleaned["command"] = parts
			}
		}
		// Skip disabled entries.
		if enabled, ok := cleaned["enabled"].(bool); ok && !enabled {
			continue
		}
		delete(cleaned, "enabled")

		result[name] = cleaned
	}
	return result
}

// BuildCapabilitiesJSON reads the user's opencode config and builds a
// dynamic capabilities JSON string for the runtime adapter. It includes
// MCP server names and model providers from the user's config, alongside
// the base tools opencode always supports. Returns the JSON as a string.
//
// The result changes when the user updates their opencode config (adds a
// new MCP server, configures a new provider) — callers should re-read on
// each heartbeat rather than caching permanently.
func BuildCapabilitiesJSON() string {
	caps := map[string]any{
		"tools":     []string{"file_edit", "terminal", "web_fetch", "git", "glob", "grep"},
		"context":   []string{"file_index"},
		"execution": []string{"checkpoint", "pause_resume", "cancellation"},
		"telemetry": []string{"tool_calls_streamed", "file_diffs"},
	}

	cfg := readOpenCodeConfig(nil)
	if cfg == nil {
		b, _ := json.Marshal(caps)
		return string(b)
	}

	// MCP servers — list enabled ones by name.
	if mcpRaw, ok := cfg["mcp"]; ok {
		if mcp, ok := mcpRaw.(map[string]any); ok {
			names := make([]string, 0, len(mcp))
			for name, entry := range mcp {
				if e, ok := entry.(map[string]any); ok {
					if enabled, ok := e["enabled"].(bool); ok && !enabled {
						continue
					}
				}
				names = append(names, name)
			}
			// Also check mcpServers for backward compat.
			if mcpServersRaw, ok := cfg["mcpServers"]; ok {
				if mcpServers, ok := mcpServersRaw.(map[string]any); ok {
					for name := range mcpServers {
						if !contains(names, name) {
							names = append(names, name)
						}
					}
				}
			}
			sort.Strings(names)
			caps["mcp_servers"] = names
		}
	}

	// Model providers.
	if providersRaw, ok := cfg["provider"]; ok {
		if providers, ok := providersRaw.(map[string]any); ok {
			names := make([]string, 0, len(providers))
			for name := range providers {
				names = append(names, name)
			}
			sort.Strings(names)
			caps["model_providers"] = names
		}
	}

	b, _ := json.Marshal(caps)
	return string(b)
}

func contains(slice []string, s string) bool {
	for _, v := range slice {
		if v == s {
			return true
		}
	}
	return false
}

