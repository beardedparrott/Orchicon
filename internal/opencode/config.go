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

// ScratchDir is the single directory workers may use outside the project
// for ephemeral scratch (screenshots, logs, downloaded files). It is the
// primary external_directory carve-out: a precise `/tmp/orchicon/**` allow,
// so the supervisor socket (/tmp/orchicon-agent.sock), the execution guard
// shims (/tmp/orchicon-guard-*), and the opencode-data dirs
// (/tmp/opencode-data-*, which hold the seeded model auth.json copies) stay
// behind the deny. Workers are told to use it in the composite prompt's
// runtime-environment block.
const ScratchDir = "/tmp/orchicon"

// OrchiconRunDirGlob is the external_directory carve-out for Orchicon's
// own run metadata (`.orchicon/<run>/` under the project root, and any
// `.orchicon/worker.recovery` / run summary files). Workflow step workers
// run in an isolated worktree that is a SIBLING of the run `.orchicon/`
// directory, so without this allow they hit an external_directory deny on
// every read the composite prompt explicitly tells them to make (summary,
// facts_learned, issues) — each block burns a full tool call + retry. The
// carve-out is tight: only the `.orchicon/` subtree (Orchicon-owned,
// aliased run metadata, gitignored), never the supervisor socket or the
// auth/data dirs elsewhere on disk.
const orchidsRunDirPattern = "**/.orchicon/**"

// taskToolDeny denies opencode's built-in `task` (subagent) tool for every
// worker execution. Orchicon already splits work into focused per-worker
// steps; a subagent that opencode spawns re-prepends its own system prompt
// and re-carries the parent's history, roughly DOUBLING context on that
// turn. This rule removes the surface entirely (permission layers gate the
// built-in `task` tool the same way they gate `bash`/`edit`). Denied via a
// "*" catch-all so no subagent plan can be approved.
const taskToolDeny = "task"

// readGrepDeny doubles as the composite-tool carve-out: when orchicon's
// worktree batch tools are enabled (ConfigOptions.CompositeTools), the
// built-in `read` and `grep` tools are denied so the worker MUST use the
// composite batch_read / batch_grep — which is what collapses the turn count
// and the re-sent-context cost. Denying the built-in tools here is safe
// because the composite MCP sidecar is registered alongside, so a worker
// always has a working read/grep path.
const (
	readToolDeny = "read"
	grepToolDeny = "grep"
)

// permissionRules builds the opencode `permission` config injected into every
// worker execution. Rules with an explicit "deny" are enforced even when the
// adapter spawns opencode with --auto (auto-approval only affects "ask"), so a
// compromised or misbehaving worker cannot run commands that damage the host.
//
// external_directory is the primary guard: it is triggered by any tool that
// reads or writes a path outside the project working directory (read, edit,
// glob, grep, and path-carrying bash commands). It is denied by default with
// two precise carve-outs — the scratch subtree and the run-metadata subtree —
// so a worker may read/write ephemeral scratch and Orchicon's own run metadata
// but nothing else outside the project. opencode evaluates the rules in order
// with the LAST matching rule winning, so the catch-all "*" deny comes first
// and the specific allows override it only for those subtrees (see
// config_permission_test.go).
//
// The bash deny list is the second layer. opencode matches each rule against
// the command string, so these only see the exact command the Bash tool runs:
// a destructive command issued from inside a subprocess (a python TUI,
// os.system, subprocess.run) is invisible here. That hole is closed by the
// OS-level execution guard (guard.go), which shims dangerous binaries on the
// worker's PATH and catches the command no matter where it is spawned. The
// rules below are belt-and-suspenders for the direct-Bash-tool case: the
// destructive-class `rm` targets (/, system paths, ~, $HOME, --no-preserve-
// root, current-dir wipes — in-project cleanup is allowed), sudo escalation,
// disk/partition/LVM tools, the shell-construct smuggling variants
// (`(rm -rf /) &`, `{ rm -rf /; }`, chained `;`/`&&`/`&`/`|`), device
// redirection, root-wide chmod/chown, and download-and-execute.
//
// There is intentionally no catch-all "*" rule here: unmatched commands fall
// back to opencode's default (ask, which --auto approves) instead of letting a
// broad allow rule win by ordering.
func permissionRules(compositeTools bool) map[string]any {
	bashDeny := []string{
		// rm family — target-scoped. In-project cleanup (`rm -rf build/`,
		// `node_modules`, `.next`) is legitimate and no longer denied (the
		// denial burned worker tokens on `find -delete`/python workarounds);
		// the OS-level execution guard is the precise backstop (it allows rm
		// only when every path stays inside the project + scratch). What stays
		// denied is the destructive class: absolute system paths, /, ~, $HOME,
		// --no-preserve-root, and the current-dir-wipe variants — the
		// commands that escape the project no matter how they're written.
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
	perm := map[string]any{
		"external_directory": map[string]any{
			"*":                  "deny",
			ScratchDir + "/**":   "allow",
			orchidsRunDirPattern: "allow",
		},
		"bash": rules,
		// Deny the subagent tool (see taskToolDeny).
		taskToolDeny: map[string]any{"*": "deny"},
	}
	if compositeTools {
		// Force the worker onto the composite batch_read/batch_grep tools by
		// denying the granular built-ins. The composite MCP sidecar is always
		// registered alongside (see BuildConfigContent), so a worker still has
		// a working read/grep path — it just batching in one call.
		perm[readToolDeny] = map[string]any{"*": "deny"}
		perm[grepToolDeny] = map[string]any{"*": "deny"}
	}
	return perm
}

// ConfigOptions configures the opencode config document injected via
// OPENCODE_CONFIG_CONTENT.
type ConfigOptions struct {
	// AgentName is the agent id in the config (e.g. "orchicon-worker").
	AgentName string
	// AgentPrompt is the system prompt text for that agent (empty = no
	// custom agent; opencode uses its default agent).
	AgentPrompt string
	// DefaultAgent names the primary agent sessions fall back to when none
	// is specified. Orchicon registers a minimal `orchicon-worker` prompt
	// and sets this so every session uses it instead of opencode's built-in
	// `build` agent (whose large default prompt is redundant with Orchicon's
	// own per-message system prompt). Empty = opencode's default (`build`).
	DefaultAgent string
	// ModelRef is the model reference the agent runs with.
	ModelRef string
	// TenantID scopes the built-in Orchicon MCP server to a tenant. It
	// is combined with OrchiconMCP: both must be set to register it.
	TenantID string
	// OrchiconMCP registers the built-in Orchicon MCP server (spawns this
	// binary's `mcp` subcommand with ORCHICON_MCP_TENANT_ID set) so every
	// opencode run has Orchicon's tools natively. Disable ONLY for
	// executions that cannot reach the plane's Postgres — workflow runtime
	// containers are isolated sandboxes with no DB route (except the
	// in-sandbox plane on :orchicon-dev, where MCPEnv points the sidecar
	// at the container-local Postgres).
	OrchiconMCP bool
	// MCPEnv are extra environment variables merged into the built-in
	// Orchicon MCP server's `environment` map (on top of the tenant id).
	// The runtime-container sandbox uses it to point `orchicon mcp` at the
	// in-container Postgres (ORCHICON_POSTGRES_DSN) so workers get
	// `orchicon_*` tools against the sandbox DB, never the host plane's.
	MCPEnv map[string]string
	// MCPBinaryPath overrides the orchicon binary path in the built-in MCP
	// server's command (defaults to the current executable). The
	// runtime-container sandbox forces /usr/local/bin/orchicon — the
	// daemon's bind-mount, guaranteed present in every runtime container —
	// because the plane's own executable path (which builds the config) is
	// not necessarily present inside the container.
	MCPBinaryPath string
	// PlaneMCP registers the plane-channel Orchicon MCP server
	// (`orchicon-plane`): the `orchicon mcp` sidecar runs in API-client
	// mode against the REAL instance's Connect API (ORCHICON_PLANE_URL)
	// using a short-lived, role-scoped worker credential
	// (ORCHICON_PLANE_TOKEN) — no Postgres DSN, no sandbox. Deny-by-default:
	// only role-bound published workers get the channel (the runtime
	// lifecycle mints the credential and threads it through planeEnv).
	PlaneMCP bool
	// PlaneMCPEnv are the environment variables for the plane-channel MCP
	// server (ORCHICON_PLANE_URL, ORCHICON_PLANE_TOKEN, tenant + run ids).
	PlaneMCPEnv map[string]string
	// SkipUserMCP omits the operator's opencode-config MCP servers from the
	// injected config. Required for the runtime-container serve: a SERVE
	// eagerly connects to every configured MCP server at startup, and the
	// operator's entries (e.g. an `orchicon` MCP that `docker exec`s into
	// a container, or a local node-based Playwright MCP) cannot run inside
	// the sandbox — an unresolvable MCP hangs the serve's event loop, so
	// the published port never answers. (The removed one-shot `opencode
	// run` path tolerated MCP failures; a serve cannot.)
	SkipUserMCP bool
	// CompositeTools registers the Orchicon worktree MCP server
	// (`orchicon-worktree`, spawning this binary's `mcp` subcommand with
	// ORCHICON_MCP_WORKTREE_DIR set) so a worker can call the composite
	// context-efficient file tools (batch_read / batch_grep / batch_write)
	// instead of opencode's granular read/grep/write tools. It also denies
	// the built-in `read` and `grep` tools (see permissionRules), so the
	// worker is forced onto the batch tools — which is what collapses the
	// number of turns. Only set alongside WorktreeDir.
	CompositeTools bool
	// WorktreeDir is the base directory the composite worktree MCP server
	// resolves its paths against: the worker's project/worktree directory. It
	// is injected as the sidecar's ORCHICON_MCP_WORKTREE_DIR env var.
	WorktreeDir string
}

// BuildConfigContent builds the JSON string for the OPENCODE_CONFIG_CONTENT
// env var. It merges the agent configuration with MCP servers from the
// user's opencode config file AND the built-in Orchicon MCP server (when
// enabled), so worker executions and the Ask Orchicon chat automatically
// get both the user's MCP tools and Orchicon's own tools.
//
// The built-in Orchicon MCP entry spawns this binary's `mcp` subcommand
// over stdio (opencode names its tools `orchicon_<tool>`) with the tenant
// injected through the server's `environment` map — proper MCP, no
// text-protocol tool emulation.
func BuildConfigContent(o ConfigOptions) string {
	cfg := map[string]any{
		"$schema": "https://opencode.ai/config.json",
	}

	if o.AgentPrompt != "" {
		cfg["agent"] = map[string]any{
			o.AgentName: map[string]any{
				"prompt": o.AgentPrompt,
				"mode":   "primary",
				"model":  o.ModelRef,
			},
		}
	}
	// Default the session to the registered agent (e.g. orchicon-worker) so
	// workers do NOT run under opencode's built-in `build` agent prompt.
	// The worker's real system prompt still rides the per-message `system`
	// field; the default agent is a MINIMAL tool-guideline shell that
	// replaces opencode's large built-in prompt (a big per-turn token win).
	if o.DefaultAgent != "" {
		cfg["default_agent"] = o.DefaultAgent
	}
	// Inject the worker's cross-cutting rule file (worker.md) as dedicated
	// instructions rather than letting the model discover/read it on demand.
	// opencode combines these with the auto-loaded AGENTS.md router, so a
	// worker gets the correct rules deterministically and never needs to (and
	// is told not to) read developer.md — closing the gap where a worker
	// self-classified as a "developer" and pulled the wrong file. The path is
	// resolved relative to the session cwd (the worktree), where worker.md is
	// tracked. Ask Orchicon uses a separate session path and is unaffected.
	if o.DefaultAgent == workerAgent {
		cfg["instructions"] = []string{"worker.md"}
	}

	// Merge MCP servers: the user's own opencode-config servers first,
	// then the built-in Orchicon MCP (unless the user already defines one
	// named `orchicon` — respect their explicit choice). SkipUserMCP
	// omits the user's servers entirely (runtime-container serve).
	mcp := map[string]any{}
	if !o.SkipUserMCP {
		if mcpServers := readMCPServers(slog.Default()); len(mcpServers) > 0 {
			for k, v := range mcpServers {
				mcp[k] = v
			}
		}
	}
	if o.OrchiconMCP && o.TenantID != "" {
		if _, exists := mcp["orchicon"]; !exists {
			mcp["orchicon"] = orchiconMCPServer(o.TenantID, o.MCPEnv, o.MCPBinaryPath)
		}
	}
	if o.PlaneMCP {
		if _, exists := mcp["orchicon-plane"]; !exists {
			mcp["orchicon-plane"] = planeMCPServer(o.PlaneMCPEnv, o.MCPBinaryPath)
		}
	}
	if o.CompositeTools && o.WorktreeDir != "" {
		if _, exists := mcp["orchicon-worktree"]; !exists {
			mcp["orchicon-worktree"] = worktreeMCPServer(o.WorktreeDir, o.MCPBinaryPath)
		}
	}
	if len(mcp) > 0 {
		cfg["mcp"] = mcp
	}

	// Inject the hard permission deny rules so every worker execution is
	// sandboxed to its project directory regardless of --auto mode. When the
	// composite worktree tools are enabled AND a worktree dir resolved, the
	// built-in read/grep are denied too so the worker is forced onto
	// batch_read/batch_grep. The WorktreeDir guard guarantees the deny is
	// only applied alongside a registered worktree MCP — so a worker never
	// loses file access with no batch tool to fall back on.
	cfg["permission"] = permissionRules(o.CompositeTools && o.WorktreeDir != "")

	// Enable context compaction pruning for every worker session. `prune`
	// trims OLD tool outputs (accumulating command/file-read results) from
	// the conversation when the window nears its limit, so a long step does
	// not keep re-sending every past `read`/`grep`/`bash` result on every
	// turn. It is capability-safe: it removes stale tool RESULTS, not the
	// tool definitions or the model's working instructions.
	//
	// `auto` is set FALSE explicitly. It is opencode's OWN lossy
	// auto-compaction (collapsing the conversation to a summary when the
	// window fills), which is redundant — and harmful — alongside Orchicon's
	// budget-ladder compaction. On a large-window SOTA model (e.g. DeepSeek
	// with a 1M context) `auto` would never normally fire, but leaving it at
	// opencode's default risks a second, independent compaction driver
	// interrupting the worker mid-flight on top of Orchicon's own. Orchicon
	// is the single source of compaction decisions (its ladder + the
	// turn-count hygiene gate). Enabling `prune` directly cuts the
	// accumulated-output token cost across a step; `auto: false` guarantees
	// opencode does not independently collapse the session.
	cfg["compaction"] = map[string]any{"prune": true, "auto": false}

	// Let a worker ingest a large file in ONE `read` instead of being forced
	// to chunk it across many small calls. opencode's default tool-output cap
	// (~50KB) truncates big `read`/`bash` results, so a model that needs a
	// large file keeps re-issuing small reads over many turns — and every
	// extra turn re-sends the whole accumulated context (the dominant cost).
	// Raising the cap trades one big context append for many re-sends.
	// Orchicon's own capToolOutput (maxToolOutputBytes, default 128k) still
	// bounds the DURABLE transcript persisted for recovery/follow-ups, so the
	// two caps are independent: opencode controls what the model per-turn
	// sees, Orchicon controls what is stored.
	cfg["tool_output"] = map[string]any{"max_bytes": 512000, "max_lines": 5000}

	// Ask opencode to batch independent tool calls into a single assistant
	// turn rather than emit them one at a time. Being explicit about batching
	// (rather than only prompting for it) collapses the number of model
	// round-trips, which is the main per-turn cost lever for a context-heavy
	// task. Experimental in opencode; harmless if the provider/model cannot
	// batch (it falls back to sequential calls).
	cfg["experimental"] = map[string]any{"batch_tool": true}

	b, err := json.Marshal(cfg)
	if err != nil {
		slog.Default().Warn("opencode: marshal config content", "error", err)
		// Fall back to minimal config.
		fallback := map[string]any{
			"$schema": "https://opencode.ai/config.json",
		}
		if o.AgentPrompt != "" {
			fallback["agent"] = map[string]any{
				o.AgentName: map[string]any{
					"prompt": o.AgentPrompt,
					"mode":   "primary",
					"model":  o.ModelRef,
				},
			}
		}
		if len(mcp) > 0 {
			fallback["mcp"] = mcp
		}
		fallback["permission"] = permissionRules(o.CompositeTools && o.WorktreeDir != "")
		b, _ = json.Marshal(fallback)
	}
	return string(b)
}

// orchiconBinaryPath returns the path to the orchicon executable used to
// spawn the Orchicon MCP sidecar. Prefers the current executable (the
// control plane IS the orchicon binary, in-container included) and falls
// back to "orchicon" on PATH.
func orchiconBinaryPath() string {
	if exe, err := os.Executable(); err == nil && exe != "" {
		return exe
	}
	return "orchicon"
}

// orchiconMCPServer builds the opencode MCP config entry for the built-in
// Orchicon MCP server (opencode v1 schema: type "local", command array,
// environment map, enabled). The tenant flows to the sidecar through the
// environment map; the Postgres DSN is inherited from the plane's env
// unless overridden via extraEnv (the runtime-container sandbox points the
// sidecar at its in-container Postgres this way). binaryPath overrides the
// orchicon binary used as the MCP command (defaults to the current
// executable) — the sandbox forces the daemon's runtime-container mount.
func orchiconMCPServer(tenantID string, extraEnv map[string]string, binaryPath string) map[string]any {
	env := map[string]string{"ORCHICON_MCP_TENANT_ID": tenantID}
	for k, v := range extraEnv {
		env[k] = v
	}
	if binaryPath == "" {
		binaryPath = orchiconBinaryPath()
	}
	return map[string]any{
		"type":        "local",
		"command":     []string{binaryPath, "mcp"},
		"environment": env,
		"enabled":     true,
		"timeout":     15000,
	}
}

// planeMCPServer builds the opencode MCP config entry for the plane
// channel (`orchicon-plane`): the `orchicon mcp` sidecar runs in
// API-client mode against the real instance (ORCHICON_PLANE_URL) with the
// run's scoped worker credential (ORCHICON_PLANE_TOKEN). The sidecar
// selects the plane registry from its env (see cmd/orchicon runMCP).
func planeMCPServer(extraEnv map[string]string, binaryPath string) map[string]any {
	env := map[string]string{}
	for k, v := range extraEnv {
		env[k] = v
	}
	if binaryPath == "" {
		binaryPath = orchiconBinaryPath()
	}
	return map[string]any{
		"type":        "local",
		"command":     []string{binaryPath, "mcp"},
		"environment": env,
		"enabled":     true,
		"timeout":     15000,
	}
}

// worktreeMCPServer builds the opencode MCP config entry for the composite
// worktree tools (batch_read / batch_grep / batch_write). It spawns this
// binary's `mcp` subcommand with ORCHICON_MCP_WORKTREE_DIR set, which makes
// runMCP select the worktree registry (no Postgres needed) bound to the
// worker's project/worktree directory. binaryPath overrides the orchicon
// binary used as the command (the runtime container forces the daemon mount).
func worktreeMCPServer(dir, binaryPath string) map[string]any {
	if binaryPath == "" {
		binaryPath = orchiconBinaryPath()
	}
	return map[string]any{
		"type":    "local",
		"command": []string{binaryPath, "mcp"},
		"environment": map[string]string{
			"ORCHICON_MCP_WORKTREE_DIR": dir,
		},
		"enabled": true,
		"timeout": 15000,
	}
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
