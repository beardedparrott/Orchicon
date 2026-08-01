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
	if err := json.Unmarshal(stripJSONC(data), &cfg); err != nil {
		log.Warn("opencode: cannot parse config file", "path", path, "error", err)
		return nil
	}
	return cfg
}

// stripJSONC removes // and /* */ comments and trailing commas from a
// JSONC document while preserving string contents. opencode config files
// (.jsonc) routinely contain comments and trailing commas that strict
// json.Unmarshal rejects. Two passes: comments first, then trailing commas
// (a comment may sit between a comma and the closing brace).
func stripJSONC(data []byte) []byte {
	return stripTrailingCommas(stripJSONCComments(data))
}

// stripJSONCComments removes // and /* */ comments (string-aware).
func stripJSONCComments(data []byte) []byte {
	var out []byte
	inString, escaped := false, false
	for i := 0; i < len(data); i++ {
		c := data[i]
		if inString {
			out = append(out, c)
			if escaped {
				escaped = false
			} else if c == '\\' {
				escaped = true
			} else if c == '"' {
				inString = false
			}
			continue
		}
		switch c {
		case '"':
			inString = true
			out = append(out, c)
		case '/':
			if i+1 < len(data) && data[i+1] == '/' {
				for i < len(data) && data[i] != '\n' {
					i++
				}
				if i < len(data) {
					out = append(out, '\n')
				}
			} else if i+1 < len(data) && data[i+1] == '*' {
				i += 2
				for i+1 < len(data) && !(data[i] == '*' && data[i+1] == '/') {
					i++
				}
				i++
			} else {
				out = append(out, c)
			}
		default:
			out = append(out, c)
		}
	}
	return out
}

// stripTrailingCommas removes ",}" and ",]" (string-aware).
func stripTrailingCommas(data []byte) []byte {
	var out []byte
	inString, escaped := false, false
	for i := 0; i < len(data); i++ {
		c := data[i]
		if inString {
			out = append(out, c)
			if escaped {
				escaped = false
			} else if c == '\\' {
				escaped = true
			} else if c == '"' {
				inString = false
			}
			continue
		}
		if c == ',' {
			j := i + 1
			for j < len(data) && (data[j] == ' ' || data[j] == '\t' || data[j] == '\n' || data[j] == '\r') {
				j++
			}
			if j < len(data) && (data[j] == '}' || data[j] == ']') {
				continue
			}
		}
		out = append(out, c)
	}
	return out
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

// permissionRules builds the opencode `permission` config injected into every
// worker execution. Rules with an explicit "deny" are enforced even when the
// adapter spawns opencode with --auto (auto-approval only affects "ask"), so a
// compromised or misbehaving worker cannot run commands that damage the host.
//
// external_directory is the primary guard: it is triggered by any tool that
// reads or writes a path outside the project working directory (read, edit,
// glob, grep, and path-carrying bash commands). Denying it hard-scopes every
// worker to its --dir project directory, which is the scoping Orchicon
// documents for workers (workers must operate within their assigned project).
//
// The bash deny list is the second layer. opencode matches each rule against
// the command string, so these only see the exact command the Bash tool runs:
// a destructive command issued from inside a subprocess (a python TUI,
// os.system, subprocess.run) is invisible here. That hole is closed by the
// OS-level execution guard (guard.go), which shims dangerous binaries on the
// worker's PATH and catches the command no matter where it is spawned. The
// rules below are belt-and-suspenders for the direct-Bash-tool case, covering
// recursive/forced deletes, sudo escalation, disk/partition/LVM tools, the
// shell-construct smuggling variants (`(rm -rf /) &`, `{ rm -rf /; }`, chained
// `;`/`&&`/`&`/`|`), device redirection, root-wide chmod/chown, and
// download-and-execute.
//
// There is intentionally no catch-all "*" rule here: unmatched commands fall
// back to opencode's default (ask, which --auto approves) instead of letting a
// broad allow rule win by ordering.
func permissionRules() map[string]any {
	bashDeny := []string{
		// rm family — direct and absolute-path invocations.
		"rm", "rm *", "rm -rf *", "rm -fr *", "rm -R *", "rm -r *", "rm -f *",
		"rm -Rf *", "rm -fR *", "rm -frr *",
		"rm -rf /", "rm -r /", "rm -R /", "rm -f /", "rm -fr /", "rm -Rf /",
		"rm -rf /*", "rm -fr /*", "rm -r /*", "rm -R /*", "rm -f /*",
		"rm -rf /home/*", "rm -rf /root/*", "rm -rf /etc/*", "rm -rf /usr/*",
		"rm -rf /var/*", "rm -rf /bin/*", "rm -rf /boot/*",
		"rm -rf ~", "rm -rf ~/*", "rm -rf $HOME", "rm -rf $HOME/*",
		"rm -rf ${HOME}/*", "rm -rf ${HOME}*",
		"rm --no-preserve-root *", "rm -rf --no-preserve-root *",
		"/bin/rm *", "/usr/bin/rm *", "/bin/rm -rf *", "/usr/bin/rm -rf *",
		"rm -rf . /", "rm -rf . ..",
		// sudo — escalate to a root shell is never needed in-project.
		"sudo", "sudo *", "sudo su *", "sudo -i *", "sudo -s *", "sudo bash *",
		"sudo sh *", "sudo rm *", "sudo * rm *", "sudo -u * rm *",
		// shell-construct smuggling variants.
		"(*rm*", "{*rm*", "(* sudo *", "{* sudo *",
		"* & rm *", "* && rm *", "* ; rm *", "* || rm *", "* | rm *",
		"* & sudo *", "* && sudo *", "* ; sudo *", "* | sudo *",
		"* & dd *", "* && dd *", "* ; dd *",
		// disk / partition / LVM / wipe tools.
		"mkfs*", "mkfs.*", "fdisk*", "parted *", "shred *", "wipefs*",
		"mkswap *", "swapoff *", "swapon *",
		"pvcreate *", "pvremove *", "vgcreate *", "vgremove *", "vgextend *",
		"lvcreate *", "lvremove *", "lvreduce *", "lvextend *",
		"dd if=* of=/dev/*", "dd * of=/dev/*", "dd of=/dev/*",
		"* > /dev/sd*", "* >> /dev/sd*", ": > /dev/sd*",
		"echo * > /dev/sd*", "echo * >> /dev/sd*",
		"cat * > /dev/sd*", "cat * >> /dev/sd*", "cp * /dev/sd*",
		"mv * /dev/null", "cp -r * /dev/null", "cp -a * /dev/null",
		// root-wide permission changes.
		"chmod -R 777 /*", "chmod -R 777 /", "chmod -R 000 /*", "chmod -R 000 /",
		"chown -R * /*", "chown -R * /", "chmod -R 777 * /",
		// download-and-execute (arbitrary remote code).
		"curl * | sh", "curl * | bash", "curl * | sh -", "curl * | bash -",
		"curl * | zsh", "wget * | sh", "wget * | bash", "wget * | zsh",
	}
	rules := make(map[string]any, len(bashDeny))
	for _, p := range bashDeny {
		rules[p] = "deny"
	}
	return map[string]any{
		"external_directory": "deny",
		"bash":               rules,
	}
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

	// Inject the hard permission deny rules so every worker execution is
	// sandboxed to its project directory regardless of --auto mode.
	cfg["permission"] = permissionRules()

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
		fallback["permission"] = permissionRules()
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

