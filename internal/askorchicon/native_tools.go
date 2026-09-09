package askorchicon

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/domain"
	"github.com/beardedparrott/orchicon/internal/orchicon"
	"github.com/beardedparrott/orchicon/internal/tenant"
)

// native_tools.go: the provider-substrate Ask tool surface.
//
// The tool surface exposed to native Ask turns is the askorchicon product
// registry PLUS the native file/shell suite (batch_read, batch_grep,
// batch_write, read, grep, write, edit, list, glob, bash, todoread/
// todowrite) — the same tools a worker session carries on the native
// bridge path, reusing the SAME engine (orchicon.NewHostTools → the
// internal/worktree composite engine) rather than a parallel
// reimplementation: one containment boundary, one cap/truncation
// semantics, one grammar. This restores the host-serve tool parity
// promised in internal/orchicon/chatturn.go for Ask sessions.
//
// AskFileRoot defines the containment boundary: the tenant's FIRST active
// project that has a project_dir configured. Ask turns have no run
// worktree (no dispatch, no manifest), so the project dir IS the
// read+write root — in-place, mirroring HostTools' in-place semantics
// (Worktree := ProjectDir, ProjectRoot := ""). The execution guard's
// destructive-command blocklist rides bash's PATH exactly as it does for
// workers (see AskGuardEnviron).

// hostSuiteToolNames is the set of host-suite tool names the Ask surface
// exposes (mirrors orchicon.HostTools' suite exactly — parity by
// construction). todoread/todowrite are included so the composite
// prompt's todowrite-every-turn contract never surfaces an error in Ask
// turns; session todo state is an execution-scoped concept (Ask has no
// execution), so they acknowledge success without state (HostTools
// already does).
var hostSuiteToolNames = []string{
	"batch_read", "batch_grep", "batch_write",
	"read", "grep", "write", "edit",
	"list", "glob", "bash",
	"todoread", "todowrite",
}

// isHostSuiteTool reports whether name is part of the file/shell suite
// (as opposed to the askorchicon product registry).
func isHostSuiteTool(name string) bool {
	for _, n := range hostSuiteToolNames {
		if n == name {
			return true
		}
	}
	return false
}

// NativeAskTools exposes this service's product tool registry PLUS the
// native file/shell suite as the provider-substrate Ask tool surface
// (orchicon.AskToolProvider) so native Ask turns can query, read, and act
// exactly like host-serve turns. The substrate never imports this package
// (layering); the server wiring injects the returned provider into the
// native bridge via SetAskTools.
func (s *Service) NativeAskTools() orchicon.AskToolProvider {
	return &nativeAskTools{service: s}
}

// nativeAskTools adapts the askorchicon registry + the HostTools suite
// onto orchicon.AskToolProvider.
type nativeAskTools struct {
	service *Service
}

// Compile-time proof the adapter satisfies the substrate contract.
var _ orchicon.AskToolProvider = (*nativeAskTools)(nil)

// askFileRootToolName is the boundary probe tool: it reports the
// project_dir the file/shell suite is scoped to. It is adapter-local
// (it never joins the shared product registry — Add is cross-request
// state and the probe is only meaningful on the native Ask path).
const askFileRootToolName = "ask_file_root"

// askFileRootResolve is the pluggable boundary resolver behind both the
// ask_file_root tool and every host-suite execution. Production resolves
// via AskFileRoot (DB); tests stub it via askFileRootStub.
var askFileRootResolve = AskFileRoot

// askFileRootStub installs fn as the boundary resolver for the duration of
// a test and returns its restore func (test-only seam).
func askFileRootStub(fn func(ctx context.Context, pool *db.Pool) (string, error)) func() {
	old := askFileRootResolve
	askFileRootResolve = fn
	return func() { askFileRootResolve = old }
}

// AskFileRoot resolves the file/shell suite's root: the FIRST active
// project with a project_dir configured (read-only tenant tx). An Ask
// conversation has no project binding, so the suite binds to the tenant's
// first active project — the same project the system prompt's
// "Enabled projects" context names (fetchProjectContext), which keeps the
// prompt and the tool boundary in agreement. An error names the fix
// (create/set a project dir) so the model can guide the operator.
func AskFileRoot(ctx context.Context, pool *db.Pool) (string, error) {
	if pool == nil {
		return "", fmt.Errorf("ask file root: no database pool (Ask service not wired)")
	}
	tenantID := tenant.FromContext(ctx)
	if tenantID == "" {
		return "", fmt.Errorf("ask file root: no tenant in context")
	}
	ttx, err := pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return "", fmt.Errorf("ask file root: %w", err)
	}
	defer ttx.Rollback(ctx)
	rows, err := db.ListProjects(ctx, ttx.Tx, db.ListProjectsFilter{
		TenantID: tenantID,
		Status:   domain.ProjectActive,
	})
	if err != nil {
		return "", fmt.Errorf("ask file root: list projects: %w", err)
	}
	for _, p := range rows {
		if dir := strings.TrimSpace(p.ProjectDir); dir != "" {
			return dir, nil
		}
	}
	return "", fmt.Errorf("ask file root: no active project with a project_dir configured — create one with create_project / create_project_directory first")
}

// askHostToolsForRoot builds the file/shell suite scoped to root (the
// enabled project's project_dir, read+write; no separate read-only project
// root — in-place semantics). bash runs in-process WITH the execution
// guard's destructive-command shim first on PATH (AskGuardEnviron —
// worker-path parity); the suite is fresh per execution (no shared state
// across calls).
func askHostToolsForRoot(root string) *orchicon.HostTools {
	h := orchicon.NewHostTools(root, "")
	h.SetBashEnviron(AskGuardEnviron)
	return h
}

// AskToolDefs returns the combined tool surface: product tools first, then
// the file/shell suite (deduped — a product tool never shadows a host one).
// The suite requires a resolvable root; without one the defs still include
// every product tool, and ExecuteAskTool fails LOUD on file/shell calls
// naming the fix (never a silent, empty tool surface).
func (a *nativeAskTools) AskToolDefs() []orchicon.ToolDef {
	if a.service == nil {
		return nil
	}
	// Product tools from the registry (fresh slice; never mutated).
	defs := make([]orchicon.ToolDef, 0, 64)
	have := map[string]bool{}
	if a.service.toolRegistry != nil {
		for _, d := range a.service.toolRegistry.List() {
			if have[d.Name] {
				continue
			}
			have[d.Name] = true
			defs = append(defs, orchicon.ToolDef{
				Name:        d.Name,
				Description: d.Description,
				ParamsJSON:  toolParamsSchema(d),
			})
		}
	}
	// The boundary probe (ask_file_root) is adapter-local — it never joins
	// the shared product registry (Add is cross-request state).
	if !have[askFileRootToolName] {
		have[askFileRootToolName] = true
		defs = append(defs, orchicon.ToolDef{
			Name:        askFileRootToolName,
			Description: "Report the project_dir the Ask file/shell suite (batch_read/read/grep/write/edit/bash/…) is scoped to. Call it first if a file/shell tool errors so you know which directory it operates in.",
			ParamsJSON:  `{"type":"object"}`,
		})
	}
	// Host suite defs: the SAME definitions the worker path serves
	// (orchicon.HostTools.Defs(), arg shapes and all — one grammar).
	for _, d := range askHostToolsForRoot("").Defs() {
		if have[d.Name] {
			continue
		}
		have[d.Name] = true
		defs = append(defs, d)
	}
	return defs
}

// ExecuteAskTool runs one tool call. Product tools route through the
// registry; file/shell tools route through the HostTools engine scoped to
// the enabled project's project_dir. Unknown names error LOUD (never a
// silent no-op), mirroring the registry's contract.
func (a *nativeAskTools) ExecuteAskTool(ctx context.Context, name, argsJSON string) (string, error) {
	if a.service == nil {
		return "", fmt.Errorf("ask tools unavailable")
	}
	// The boundary probe: names the project_dir the suite is scoped to.
	if name == askFileRootToolName {
		root, err := askFileRootResolve(ctx, a.service.pool)
		if err != nil {
			return "", err
		}
		b, merr := json.Marshal(map[string]string{"project_dir": root})
		if merr != nil {
			return "", merr
		}
		return string(b), nil
	}
	// Host suite: the file/shell tools. The root resolves per execution
	// (fresh project state, never a stale cache) — one DB read, cheap.
	if isHostSuiteTool(name) {
		root, err := askFileRootResolve(ctx, a.service.pool)
		if err != nil {
			return "", fmt.Errorf("%s: %w", name, err)
		}
		if argsJSON == "" {
			argsJSON = "{}"
		}
		return askHostToolsForRoot(root).Execute(ctx, name, argsJSON)
	}
	// Product tools through the registry.
	if a.service.toolRegistry == nil {
		return "", fmt.Errorf("ask tools unavailable")
	}
	td, ok := a.service.toolRegistry.Get(name)
	if !ok {
		return "", fmt.Errorf("ask tool %q is not registered", name)
	}
	raw, err := td.Fn(ctx, a.service.pool, json.RawMessage(argsJSON))
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

// toolParamsSchema renders the MCP-style property map as a JSON-schema
// parameters object for the provider wire (mirrors the MCP adapter's
// property mapping).
func toolParamsSchema(d ToolDefinition) string {
	schema := map[string]any{"type": "object"}
	if len(d.Properties) > 0 {
		props := make(map[string]any, len(d.Properties))
		for k, v := range d.Properties {
			prop := map[string]any{"type": v.Type}
			if v.Type == "" {
				prop["type"] = "string"
			}
			if v.Description != "" {
				prop["description"] = v.Description
			}
			props[k] = prop
		}
		schema["properties"] = props
	}
	if len(d.Required) > 0 {
		schema["required"] = d.Required
	}
	b, err := json.Marshal(schema)
	if err != nil {
		return `{"type":"object"}`
	}
	return string(b)
}
