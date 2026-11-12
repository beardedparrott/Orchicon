package opencode

import (
	"encoding/json"
	"fmt"
	"github.com/beardedparrott/orchicon/internal/runtime"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/beardedparrott/orchicon/internal/adapter"
	"github.com/beardedparrott/orchicon/internal/mcpclient"
	"github.com/beardedparrott/orchicon/internal/neverallow"
	"github.com/beardedparrott/orchicon/internal/workerrestrict"
)

// opencodeConfigPath returns the path to the opencode config file (opencode.json or opencode.jsonc)
// by checking, in order: 1.
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

// readOpenCodeConfig reads the user's opencode config file and returns the raw map.
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

// stripJSONC removes // and /* */ comments and trailing commas from a JSONC document while
// preserving string contents.
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

// readMCPServers extracts the MCP server definitions from the user's opencode config.
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

// ScratchDir is the single directory workers may use outside the project for ephemeral scratch
// (screenshots, logs, downloaded files).
const ScratchDir = workerrestrict.ScratchDir

// OrchiconRunDirGlob is the external_directory carve-out for Orchicon's own run metadata
// (`.orchicon/<run>/` under the project root.
const orchidsRunDirPattern = workerrestrict.RunDirPattern

// taskToolDeny denies opencode's built-in `task` (subagent) tool for every worker execution.
const taskToolDeny = workerrestrict.TaskToolDeny

// readGrepDeny doubles as the composite-tool carve-out: when orchicon's worktree batch tools are
// enabled (ConfigOptions.CompositeTools).
const (
	readToolDeny = "read"
	grepToolDeny = "grep"
)

// permissionRules builds the opencode `permission` config injected into every worker execution.

// PermissionProfile selects which permission rule set the injected opencode config carries.
type PermissionProfile string

const (
	// ProfileWorker is the sandboxed worker profile: the project boundary (external_directory denied
	// with the scratch + run-metadata carve-outs), the full bash deny list, and — with composite tools.
	ProfileWorker PermissionProfile = "worker"
	// ProfileInteractive is the Ask Orchicon profile.
	ProfileInteractive PermissionProfile = "interactive"
)

// normalized maps every unrecognised value — including the zero value "" — onto ProfileWorker, so a
// caller that never set a profile gets today's behaviour rather than an accidental widening.
func (p PermissionProfile) normalized() PermissionProfile {
	if p == ProfileInteractive {
		return ProfileInteractive
	}
	return ProfileWorker
}

// permissionRules dispatches on the profile. The interactive profile ignores
// compositeTools: its read/grep tools are never denied.
func permissionRules(profile PermissionProfile, compositeTools bool) map[string]any {
	if profile.normalized() == ProfileInteractive {
		return interactivePermissionRules()
	}
	return workerPermissionRules(compositeTools)
}

// PermissionRulesForProfile returns a profile's permission subtree with the composite-tool
// carve-out OFF.
func PermissionRulesForProfile(p PermissionProfile) map[string]any {
	return permissionRules(p, false)
}

// interactivePermissionRules builds the Ask Orchicon profile (see ProfileInteractive).
func interactivePermissionRules() map[string]any {
	rules := make(map[string]any, len(neverallow.CommandPatterns))
	for _, p := range neverallow.DenyRules() {
		rules[p] = "deny"
	}
	return map[string]any{
		"external_directory": map[string]any{"*": "allow"},
		"edit":               map[string]any{"*": "ask"},
		"write":              map[string]any{"*": "ask"},
		"bash":               rules,
		// Deny the subagent tool (see taskToolDeny) in every profile.
		"task": map[string]any{"*": "deny"},
	}
}

// workerPermissionRules builds the sandboxed worker profile (see ProfileWorker): today's rules,
// byte-identical.
func workerPermissionRules(compositeTools bool) map[string]any {
	// The worker bash deny list — the shared never-allow binary class PLUS the project-boundary rules —
	// in ONE copy, in internal/workerrestrict.
	bashDeny := workerrestrict.WorkerDenyPatterns()
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
		// Force the worker onto the composite batch_read/batch_grep tools by denying the granular
		// built-ins.
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
	// DefaultAgent names the primary agent sessions fall back to when none is specified.
	DefaultAgent string
	// ModelRef is the model reference the agent runs with.
	ModelRef string
	// TenantID scopes the built-in Orchicon MCP server to a tenant. It
	// is combined with OrchiconMCP: both must be set to register it.
	TenantID string
	// Disable ONLY for executions that cannot reach the plane's Postgres — workflow runtime containers
	// are isolated sandboxes with no DB route (except the in-sandbox plane on :orchicon-dev.
	OrchiconMCP bool
	// MCPEnv are extra environment variables merged into the built-in Orchicon MCP server's
	// `environment` map (on top of the tenant id).
	MCPEnv map[string]string
	// MCPBinaryPath overrides the orchicon binary path in the built-in MCP server's command (defaults
	// to the current executable).
	MCPBinaryPath string
	// PlaneMCP registers the plane-channel Orchicon MCP server (`orchicon-plane`): the `orchicon mcp`
	// sidecar runs in API-client mode against the REAL instance's Connect API (ORCHICON_PLANE_URL).
	PlaneMCP bool
	// PlaneMCPEnv are the environment variables for the plane-channel MCP
	// server (ORCHICON_PLANE_URL, ORCHICON_PLANE_TOKEN, tenant + run ids).
	PlaneMCPEnv map[string]string
	// SkipUserMCP omits the operator's opencode-config MCP servers from the injected config.
	SkipUserMCP bool
	// CompositeTools registers the Orchicon worktree MCP server (`orchicon-worktree`, spawning this
	// binary's `mcp` subcommand with ORCHICON_MCP_WORKTREE_DIR set) so a worker can call the composite.
	CompositeTools bool
	// Providers are the tenant's provider definitions to inject as opencode's `provider` block, with
	// their base URLs ALREADY TRANSPOSED for the consumer (see ProviderConfig).
	Providers []runtime.ProviderConfig
	// WorktreeDir is the base directory the composite worktree MCP server resolves its paths against:
	// the worker's project/worktree directory.
	WorktreeDir string
	// ProjectDir is the project root, a READ-only extra scope the composite worktree MCP server may
	// reach for the run-state `.orchicon/<run>/` files and `architecture-notes/`.
	ProjectDir string
	// PermissionProfile selects the injected permission rule set (see PermissionProfile).
	PermissionProfile PermissionProfile

	// Shape selects the config DOCUMENT shape (see configshape.go).
	Shape ConfigShape

	// V2 ONLY, and empty for a v1 serve — on 1.x the same prompt rides the prompt body's `system` field
	// per message, which is why this list has no v1 meaning.
	Workers []adapter.WorkerAgent
	// WorkflowRunID is the RUN's id, threaded to the worktree MCP sidecar so it can derive the
	// run-state directory <projectDir>/.orchicon/<runID>/ its todo snapshot is written into.
	WorkflowRunID string

	// RunMCP is the RUN's MCP union (project-owned ∪ every step worker version's inline specs),
	// resolved ONCE from the run and with ${SECRET_NAME} expanded at build time.
	RunMCP []mcpclient.ScopedServer
	// RunSkills is the RUN's skill-file union (the same walk's skill half).
	RunSkills []mcpclient.InlineSkillFile
}

// BuildConfigContent builds the JSON string for the OPENCODE_CONFIG_CONTENT env var.
func BuildConfigContent(o ConfigOptions) string {
	// The document's SHAPE follows the opencode this config will run under, unless the caller states
	// one outright.
	if o.Shape == "" {
		o.Shape = configShapeResolver(slog.Default())
	}
	cfg := map[string]any{
		"$schema": "https://opencode.ai/config.json",
	}

	if o.AgentPrompt != "" {
		if o.Shape.isV2() {
			// V2 reads `agents` and its prompt field is `system`; a non-empty `system` is what REPLACES the
			// provider's base prompt, which is how a worker avoids running under opencode's own `build` prompt.
			agents := map[string]any{o.AgentName: v2AgentEntry(o.AgentPrompt, o.ModelRef)}
			// Plus one agent per step worker, each carrying that worker's REAL prompt.
			for _, w := range o.Workers {
				if w.Name == "" || w.Prompt == "" {
					continue
				}
				if _, dup := agents[w.Name]; dup {
					continue
				}
				agents[w.Name] = v2AgentEntry(w.Prompt, w.Model)
			}
			cfg["agents"] = agents
		} else {
			cfg["agent"] = map[string]any{
				o.AgentName: map[string]any{
					"prompt": o.AgentPrompt,
					"mode":   "primary",
					"model":  o.ModelRef,
				},
			}
		}
	}
	// Default the session to the registered agent (e.g. orchicon-worker) so workers do NOT run under
	// opencode's built-in `build` agent prompt.
	if o.DefaultAgent != "" {
		cfg["default_agent"] = o.DefaultAgent
	}
	// Inject the worker's cross-cutting rule file (worker.md) as dedicated instructions rather than
	// letting the model discover/read it on demand.
	if o.DefaultAgent == workerAgent {
		instr := []string{"worker.md"}
		instr = append(instr, runSkillInstructions(o)...)
		cfg["instructions"] = instr
	}

	// Merge MCP servers: the user's own opencode-config servers first, then the built-in Orchicon MCP
	// (unless the user already defines one named `orchicon` — respect their explicit choice).
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
			mcp["orchicon-worktree"] = worktreeMCPServer(o.WorktreeDir, o.ProjectDir, o.WorkflowRunID, o.MCPBinaryPath)
		}
	}
	// A container's serve is created ONCE and this config is applied ONCE (the daemon applies
	// ServeConfig only at container creation).
	for _, ss := range o.RunMCP {
		if ss.Spec.ID == "" {
			continue
		}
		if _, exists := mcp[ss.Spec.ID]; exists {
			continue // built-ins (and the user's servers) win a name clash
		}
		mcp[ss.Spec.ID] = mcpEntryFromSpec(ss.Spec)
	}
	if len(mcp) > 0 {
		if o.Shape.isV2() {
			// V2 groups servers under mcp.servers and inverts `enabled`.
			cfg["mcp"] = v2MCPBlock(mcp)
		} else {
			cfg["mcp"] = mcp
		}
	}

	// Inject the hard permission deny rules so every worker execution is sandboxed to its project
	// directory regardless of --auto mode.
	compositeTools := o.CompositeTools && o.WorktreeDir != ""
	// On a Code Mode surface (opencode 2.x) every MCP tool is callable ONLY from inside `execute`, so
	// denying the built-ins does not redirect anything.
	directCompositeTools := compositeTools && !o.Shape.isV2()
	if o.Shape.isV2() {
		// V2 is an ordered `permissions` ARRAY with renamed actions.
		cfg["permissions"] = v2PermissionRules(o.PermissionProfile, directCompositeTools)
	} else {
		cfg["permission"] = permissionRules(o.PermissionProfile, directCompositeTools)
	}

	// Enable context compaction pruning for every worker session.
	if o.Shape.isV2() {
		// `auto: false` is the load-bearing half on both versions: it is what stops opencode running its
		// OWN lossy compaction alongside Orchicon's budget ladder.
		cfg["compaction"] = map[string]any{"auto": false}
	} else {
		cfg["compaction"] = map[string]any{"prune": true, "auto": false}
	}

	// Let a worker ingest a large file in ONE `read` instead of being forced to chunk it across many
	// small calls.
	cfg["tool_output"] = map[string]any{"max_bytes": 1000000, "max_lines": 5000}

	// The tenant's providers, with base URLs transposed for this consumer.
	if len(o.Providers) > 0 {
		providers := make(map[string]any, len(o.Providers))
		for _, p := range o.Providers {
			entry := map[string]any{}
			if p.NPM != "" {
				entry["npm"] = p.NPM
			}
			opts := map[string]any{}
			if p.BaseURL != "" {
				opts["baseURL"] = p.BaseURL
			}
			if len(opts) > 0 {
				entry["options"] = opts
			}
			// A provider with neither an npm package nor a base URL carries nothing opencode can act on, so it
			// is skipped rather than emitted as a stub that would shadow a real definition of the same name.
			if len(entry) == 0 {
				continue
			}
			providers[p.ID] = entry
		}
		if len(providers) > 0 {
			if o.Shape.isV2() {
				// V2 renames the map and moves the endpoint under settings.
				cfg["providers"] = v2ProviderBlock(providers)
			} else {
				cfg["provider"] = providers
			}
		}
	}

	// Ask opencode to batch independent tool calls into a single assistant turn rather than emit them
	// one at a time.
	if !o.Shape.isV2() {
		cfg["experimental"] = map[string]any{"batch_tool": true}
	}

	b, err := json.Marshal(cfg)
	if err != nil {
		slog.Default().Warn("opencode: marshal config content", "error", err)
		// Fall back to minimal config.
		fallback := map[string]any{
			"$schema": "https://opencode.ai/config.json",
		}
		if o.AgentPrompt != "" {
			if o.Shape.isV2() {
				fallback["agents"] = map[string]any{o.AgentName: v2AgentEntry(o.AgentPrompt, o.ModelRef)}
			} else {
				fallback["agent"] = map[string]any{
					o.AgentName: map[string]any{
						"prompt": o.AgentPrompt,
						"mode":   "primary",
						"model":  o.ModelRef,
					},
				}
			}
		}
		if len(mcp) > 0 {
			if o.Shape.isV2() {
				fallback["mcp"] = v2MCPBlock(mcp)
			} else {
				fallback["mcp"] = mcp
			}
		}
		if o.Shape.isV2() {
			fallback["permissions"] = v2PermissionRules(o.PermissionProfile, directCompositeTools)
		} else {
			fallback["permission"] = permissionRules(o.PermissionProfile, directCompositeTools)
		}
		b, _ = json.Marshal(fallback)
	}
	return string(b)
}

// AUTH IS BEARER/HEADER ONLY — the spec's Headers already carry the resolved plaintext (the plane
// expands ${SECRET_NAME} at build time, before it reaches this builder).
func mcpEntryFromSpec(s mcpclient.ServerSpec) map[string]any {
	if s.TransportType() == mcpclient.TypeHTTP {
		e := map[string]any{"type": "remote", "url": s.URL, "enabled": true}
		if len(s.Headers) > 0 {
			e["headers"] = s.Headers
		}
		return e
	}
	e := map[string]any{"type": "local", "command": s.Command, "enabled": true}
	if len(s.Env) > 0 {
		e["environment"] = s.Env
	}
	return e
}

// runSkillInstructions renders the RUN's skill-file union into opencode `instructions` paths.
func runSkillInstructions(o ConfigOptions) []string {
	var out []string
	for _, s := range o.RunSkills {
		if s.Path == "" {
			continue
		}
		if s.Content == "" {
			out = append(out, s.Path)
			continue
		}
		p, err := materialiseInlineSkill(o.WorktreeDir, s)
		if err != nil {
			slog.Default().Warn("opencode: inline skill not placed — skipping",
				"skill", s.Path, "worktree", o.WorktreeDir, "error", err)
			continue
		}
		out = append(out, p)
	}
	return out
}

// materialiseInlineSkill writes one inline skill file under <worktreeDir>/.orchicon/skills/ and
// returns the path to emit as an instruction.
func materialiseInlineSkill(worktreeDir string, s mcpclient.InlineSkillFile) (string, error) {
	if worktreeDir == "" {
		return "", fmt.Errorf("no worktree dir to place the skill in")
	}
	dir := filepath.Join(worktreeDir, ".orchicon", "skills")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	name := sanitiseSkillName(s.Path)
	if name == "" {
		return "", fmt.Errorf("skill path %q yields no usable file name", s.Path)
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(s.Content), 0o644); err != nil {
		return "", err
	}
	return p, nil
}

// sanitiseSkillName reduces a skill's declared path to a stable, filesystem- safe `.md` file name.
func sanitiseSkillName(p string) string {
	base := filepath.Base(strings.TrimSpace(p))
	base = strings.TrimSuffix(base, filepath.Ext(base))
	var b strings.Builder
	for _, r := range strings.ToLower(base) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	trimmed := strings.Trim(b.String(), "-")
	for strings.Contains(trimmed, "--") {
		trimmed = strings.ReplaceAll(trimmed, "--", "-")
	}
	if trimmed == "" {
		return ""
	}
	return trimmed + ".md"
}

// orchiconBinaryPath returns the path to the orchicon executable used to spawn the Orchicon MCP
// sidecar.
func orchiconBinaryPath() string {
	if exe, err := os.Executable(); err == nil && exe != "" {
		return exe
	}
	return "orchicon"
}

// orchiconMCPServer builds the opencode MCP config entry for the built-in Orchicon MCP server
// (opencode v1 schema: type "local", command array, environment map, enabled).
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

// planeMCPServer builds the opencode MCP config entry for the plane channel (`orchicon-plane`): the
// `orchicon mcp` sidecar runs in API-client mode against the real instance (ORCHICON_PLANE_URL).
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

// worktreeMCPServer builds the opencode MCP config entry for the composite worktree tools
// (batch_read / batch_grep / batch_write).
func worktreeMCPServer(dir, projectDir, runID, binaryPath string) map[string]any {
	if binaryPath == "" {
		binaryPath = orchiconBinaryPath()
	}
	env := map[string]string{"ORCHICON_MCP_WORKTREE_DIR": dir}
	if projectDir != "" {
		env["ORCHICON_MCP_PROJECT_DIR"] = projectDir
	}
	// THE RUN ID IS WHAT LETS THE TODO PANEL SEE ANYTHING.
	if runID != "" && projectDir != "" {
		env["ORCHICON_MCP_WORKFLOW_RUN_ID"] = runID
	}
	return map[string]any{
		"type":        "local",
		"command":     []string{binaryPath, "mcp"},
		"environment": env,
		"enabled":     true,
		"timeout":     15000,
	}
}

// ReadMCPServersFromConfig reads the user's opencode config and returns
// the MCP server map. Returns nil if no config or no MCP servers found.
func ReadMCPServersFromConfig(log *slog.Logger) map[string]any {
	return readMCPServers(log)
}

// SanitizeMCPForSubprocess strips sensitive fields from MCP entries before passing them to a
// subprocess.
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

// BuildCapabilitiesJSON reads the user's opencode config and builds a dynamic capabilities JSON
// string for the runtime adapter.
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
