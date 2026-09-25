package askorchicon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/domain"
	"github.com/beardedparrott/orchicon/internal/orchicon"
	"github.com/beardedparrott/orchicon/internal/permpolicy"
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
// AskFileScopeFor resolves the file/shell suite's SESSION ANCHOR FROM THE
// CONVERSATION: its own project_dir (the project its prompt names). A
// conversation with no project falls back to the tenant's first project that
// is active and has a project_dir, but ONLY as the relative-path anchor — that
// fallback is not the conversation's own tree (see AskFileScope.FromConversation).
// Ask turns have no run worktree (no dispatch, no manifest), so the project dir
// is the relative-path anchor and the bash cwd default (Worktree := ProjectDir,
// ProjectRoot := ""), exactly like HostTools' in-place semantics.
//
// The anchor is NOT a containment boundary. The suite is UNCONFINED
// (orchicon.NewHostToolsUnrestricted): an absolute path is permitted wherever
// the operator can reach it — the interactive boundary. The anchor decides
// where a relative path lands and where bash starts, nothing more. The
// execution guard's destructive-command blocklist still rides bash's PATH
// exactly as it does for workers (see AskGuardEnviron).

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

// askToolNamePrefix is the MCP-style prefix the ASK SYSTEM PROMPT teaches the
// model to use. BuildSystemPrompt says the product tools are "named
// `orchicon_<tool>`" and renders every enumerated bullet as `orchicon_%s`
// (agent.go, chat.go) — matching the stdio MCP server's naming on the opencode
// path, where the prefix IS the real name.
//
// The NATIVE path does not work that way: this registry is keyed by BARE name
// ("list_projects", "create_work_item", …; verified 0 of 81 entries carry the
// prefix) and the wire definitions AskToolDefs sends are bare too. Nothing
// normalised between the two, so a model that followed the prompt could not call
// a single product tool:
//
//	ask tool "orchicon_list_projects" is not registered
//
// It presented as intermittent because the conversation HISTORY carried past
// successful calls as in-context examples that overrode the prompt's wording.
// When compaction collapsed that history the examples were gone, leaving only
// the prompt's prefixed form — which the dispatcher rejected outright. So the
// trigger was compaction, but the defect is this naming divergence.
const askToolNamePrefix = "orchicon_"

// normalizeAskToolName maps a model-emitted tool name onto its registry key,
// tolerating the MCP-style prefix the prompt advertises.
//
// Normalising HERE rather than in the prompt is deliberate: the prefix is the
// documented MCP naming and is the REAL name on the opencode host, so rewriting
// the prompt would misdescribe that path. Accepting both forms also rescues any
// conversation whose history already contains prefixed calls — and makes the
// tool surface immune to prompt/registry drift in either direction.
func normalizeAskToolName(name string) string {
	return strings.TrimPrefix(name, askToolNamePrefix)
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
// via AskFileScopeFor (DB); tests stub it via askFileRootStub.
var askFileRootResolve = AskFileScopeFor

// askFileRootStub installs fn as the boundary resolver for the duration of
// a test and returns its restore func (test-only seam).
func askFileRootStub(fn func(ctx context.Context, pool *db.Pool) (AskFileScope, error)) func() {
	old := askFileRootResolve
	askFileRootResolve = fn
	return func() { askFileRootResolve = old }
}

// AskFileScope is the resolved boundary for the file/shell suite: the directory
// plus the fact the consent model needs — whether that directory is the
// CONVERSATION's own project (inside ⇒ pre-approved) or only the tenant-wide
// relative-path anchor for a conversation with no project (outside ⇒ ask).
type AskFileScope struct {
	Dir              string // the directory the suite is scoped to
	ProjectID        string // the conversation's project id ("" on the fallback)
	FromConversation bool   // true ⇒ Dir came from the conversation's project
}

// askScope* are the ask_file_root envelope's "scope" values: the model and the
// consent layer both branch on this rather than re-deriving "is this mine?".
const (
	askScopeConversation = "conversation"
	askScopeTenantAnchor = "tenant_fallback"
)

// PreApprovedPath reports whether target is inside this scope's PRE-APPROVED
// directory — the operator's "inside it, writes and executions proceed without
// asking".
//
// It is the ONE read the consent core makes when it decides ask vs proceed, which
// is why it lives beside the scope value rather than in the consent layer: the
// prompt's project statement, the ask_file_root envelope and the consent
// decision must all read the SAME predicate or they drift.
//
// TRUE requires BOTH halves, and each is load-bearing:
//   - FromConversation — the directory is the CONVERSATION's own project. A
//     tenant-wide fallback anchor is not this conversation's tree, so NOTHING is
//     approved in it: an unassigned conversation asks for every write, even
//     though a directory exists to anchor relative paths. That is the whole
//     difference between "a directory" and "a project of my own".
//   - target resolving to Dir itself or a descendant of it. The comparison is on
//     cleaned paths and requires Dir + separator, so a sibling whose name merely
//     shares the prefix (…/Orchicon-v2 beside …/Orchicon) is NOT inside and asks.
//
// A relative target is resolved against Dir, matching where a relative path lands
// for the suite (the anchor is Dir). An unusable scope (no FromConversation, no
// Dir, no target) is false, never a panic and never an accidental allow: failing
// closed here means an unapproved directory asks.
func (s AskFileScope) PreApprovedPath(target string) bool {
	if !s.FromConversation || strings.TrimSpace(s.Dir) == "" || strings.TrimSpace(target) == "" {
		return false
	}
	dir := filepath.Clean(s.Dir)
	t := filepath.Clean(target)
	if !filepath.IsAbs(t) {
		t = filepath.Join(dir, t)
	}
	return t == dir || strings.HasPrefix(t, dir+string(filepath.Separator))
}

// conversationProjectRow loads the project row the prompt half
// (conversationProjectContext) and the tool half (AskFileScopeFor) both describe,
// so the two cannot drift: ONE read, ONE shape. A missing project is an error. The
// CALLER decides whether that failure is fatal — the tool layer fails loud (it must
// not hand the model a directory that is not real), while the prompt layer degrades
// to "unassigned" so a deleted project never fails the turn.
func conversationProjectRow(ctx context.Context, pool *db.Pool, tenantID, projectID string) (*db.ProjectRow, error) {
	if pool == nil {
		return nil, fmt.Errorf("conversation project: no database pool")
	}
	ttx, err := pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	defer ttx.Rollback(ctx)
	p, err := db.GetProject(ctx, ttx.Tx, tenantID, projectID)
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// AskFileScopeFor resolves the file/shell suite's boundary FROM THE CONVERSATION.
//
// The turn carries the conversation's project id on its context (chat.go stamps it
// with withAskConversationProject beside the mode and the conversation id), so the
// suite binds to the SAME project the system prompt's "## This conversation's
// project" block names — the two halves agree by construction, from one read of one
// row (conversationProjectRow).
//
// An UNASSIGNED conversation (no project id) falls back to the tenant's first project
// that is active and has a project_dir, but ONLY as the relative-path anchor:
// FromConversation stays false, so the probe envelope and the consent layer that
// follows can tell the anchor from the conversation's own tree.
//
// Status is not filtered on the conversation's own project: an archived project's dir
// still scopes the suite, because the prompt names it too — a filter here would
// silently re-open the very divergence this closes.
//
// Resolution runs once per call and is never cached across turns: the conversation's
// project can change between turns, and a stale root is the defect this closes.
func AskFileScopeFor(ctx context.Context, pool *db.Pool) (AskFileScope, error) {
	if pool == nil {
		return AskFileScope{}, fmt.Errorf("ask file root: no database pool (Ask service not wired)")
	}
	tenantID := tenant.FromContext(ctx)
	if tenantID == "" {
		return AskFileScope{}, fmt.Errorf("ask file root: no tenant in context")
	}
	if projectID := askConversationProjectFromContext(ctx); projectID != "" {
		p, err := conversationProjectRow(ctx, pool, tenantID, projectID)
		if err != nil {
			return AskFileScope{}, fmt.Errorf("ask file root: this conversation's project %q: %w", projectID, err)
		}
		dir := strings.TrimSpace(p.ProjectDir)
		if dir == "" {
			return AskFileScope{}, fmt.Errorf("ask file root: this conversation's project %q has no project directory configured — set one with create_project_directory before using the file/shell suite", projectID)
		}
		return AskFileScope{Dir: dir, ProjectID: projectID, FromConversation: true}, nil
	}
	// Unassigned: the tenant's first project that is active and carries a dir is
	// only the relative-path anchor. It is NOT this conversation's project, so
	// FromConversation stays false.
	ttx, err := pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return AskFileScope{}, fmt.Errorf("ask file root: %w", err)
	}
	defer ttx.Rollback(ctx)
	rows, err := db.ListProjects(ctx, ttx.Tx, db.ListProjectsFilter{
		TenantID: tenantID,
		Status:   domain.ProjectActive,
	})
	if err != nil {
		return AskFileScope{}, fmt.Errorf("ask file root: list projects: %w", err)
	}
	for _, p := range rows {
		if dir := strings.TrimSpace(p.ProjectDir); dir != "" {
			return AskFileScope{Dir: dir}, nil
		}
	}
	return AskFileScope{}, fmt.Errorf("ask file root: this conversation has no project and the tenant has no active project with a project_dir configured — create one with create_project / create_project_directory first")
}

// AskFileRoot reports just the boundary directory (kept for callers that only
// need the path).
func AskFileRoot(ctx context.Context, pool *db.Pool) (string, error) {
	s, err := AskFileScopeFor(ctx, pool)
	return s.Dir, err
}

// askHostToolsForRoot builds the file/shell suite for an Ask turn: the suite is
// UNCONFINED (the interactive boundary — any absolute path the operator can
// reach), while root stays its relative-path anchor and bash cwd, so
// project-relative work is unchanged. bash runs in-process WITH the execution
// guard's destructive-command shim first on PATH (AskGuardEnviron —
// worker-path parity); the suite is fresh per execution (no shared state
// across calls).
func askHostToolsForRoot(root string) *orchicon.HostTools {
	h := orchicon.NewHostToolsUnrestricted(root)
	h.SetBashEnviron(AskGuardEnviron)
	// The operator's DURABLE permission policy, consulted through the ONE
	// shared accessor (permpolicy.Store) before any call in the suite is
	// dispatched. It is the consent core's read of the same file the guard
	// shim and the plane API read, so enforcement and prompting cannot
	// drift: a denied path is refused HERE by the shared accessor, and the
	// refusal names the entry that denied it.
	h.SetPathPolicy(askPermissionPolicy().HostSuiteGuard())
	return h
}

// askPermissionPolicy is the consent core's handle on the persistent
// permission policy. A fresh Store per call, holding only the path: the
// reload semantics are "read on each consult", so there is nothing to
// memoise and nothing to invalidate (a hand-edit and a UI write are both
// live on the next gated decision).
func askPermissionPolicy() *permpolicy.Store {
	return permpolicy.NewStore(permpolicy.DefaultPath())
}

// AskToolDefs returns the combined tool surface: product tools first, then
// the file/shell suite (deduped — a product tool never shadows a host one).
// The suite requires a resolvable root; without one the defs still include
// every product tool, and ExecuteAskTool fails LOUD on file/shell calls
// naming the fix (never a silent, empty tool surface).
//
// THE MODE'S BOUNDARY IS APPLIED TO THE LIST AS WELL AS TO THE CALL.
//
// Refusing a call is the part that makes "no matter what the user says" true, and it is not enough on its own: a
// model that is OFFERED write in Brainstorm will try it, be refused, and can be talked into trying again, and
// every attempt is a wrong turn in the transcript. Filtering the defs means the model never sees the tool, so the
// tool LIST states the boundary before any call is made — and a mode that cannot do the work no longer advertises
// that it can.
//
// The filter is the SAME table the refusal uses (modeAllowsTool), so the offered surface and the enforced surface
// cannot drift: there is no second list of "tools this mode hides".
func (a *nativeAskTools) AskToolDefs(ctx context.Context) []orchicon.ToolDef {
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
			Description: "Report the directory the Ask file/shell suite (batch_read/read/grep/write/edit/bash/…) uses for THIS conversation, and whether it is the conversation's own project (scope: conversation — its default scope, PRE-APPROVED, so writes and executions inside it proceed without asking) or only the tenant-wide fallback anchor for a conversation with no project (scope: tenant_fallback — NOT approved, so every write there asks the user first). Reads never ask anywhere. Call it first if a file/shell tool errors so you know which directory it uses and whether writing there needs consent.",
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

	// DROP WHAT THIS MODE MAY NOT RUN — see the doc comment. Built as a new slice rather than filtered in place,
	// because `defs` is returned to a caller that keeps it for the turn and mutating it would be a surprise.
	mode := askModeFromContext(ctx)
	offered := make([]orchicon.ToolDef, 0, len(defs))
	for _, d := range defs {
		if ok, _ := modeAllowsTool(mode, d.Name); ok {
			offered = append(offered, d)
		}
	}
	return offered
}

// ExecuteAskTool runs one tool call. Product tools route through the
// registry; file/shell tools route through the HostTools engine scoped to
// the enabled project's project_dir. Unknown names error LOUD (never a
// silent no-op), mirroring the registry's contract.
func (a *nativeAskTools) ExecuteAskTool(ctx context.Context, name, argsJSON string) (string, error) {
	if a.service == nil {
		return "", fmt.Errorf("ask tools unavailable")
	}
	// Tolerate the MCP-style prefix the system prompt advertises BEFORE any name
	// comparison below (see askToolNamePrefix): the model is told
	// `orchicon_list_projects` while the registry is keyed `list_projects`, so
	// without this every product-tool call fails as "not registered".
	name = normalizeAskToolName(name)
	// THE MODE BOUNDARY, AND IT RUNS BEFORE EVERY OTHER BRANCH.
	//
	// Ahead of the boundary probe and the host-suite dispatch on purpose: this is the single choke point every
	// Ask tool call passes through (chatturn.go), so a check here cannot be bypassed by which internal branch a
	// tool happens to take. The operator's requirement is that a mode NEVER does the work it is not supposed to
	// do "no matter what the user says" — and no amount of asking is a substitute for the call being refused.
	//
	// THE ERROR IS THE MESSAGE. The bridge turns a tool error into the call's RESULT (chatturn.go: the text
	// becomes the content, flagged as an error), so the model is handed the refusal verbatim and can relay it
	// to the user — including the part that names the mode to switch to and says it cannot do that itself.
	if ok, refusal := modeAllowsTool(askModeFromContext(ctx), name); !ok {
		return "", errors.New(refusal)
	}
	// The boundary probe: names the project_dir the suite is scoped to AND whether that directory is this
	// conversation's own project or only the tenant-wide anchor — the scope the consent layer keys on. The
	// envelope must not present a fallback anchor as the conversation's own tree.
	if name == askFileRootToolName {
		scope, err := askFileRootResolve(ctx, a.service.pool)
		if err != nil {
			return "", err
		}
		env := map[string]string{"project_dir": scope.Dir}
		// A DENIED directory is stated as denied, and the statement names the
		// entry: the probe is what the model calls first, so "this directory
		// is refused by policy" belongs here rather than only at the first
		// failed call.
		deniedEntry := ""
		if d, derr := askPermissionPolicy().Decide(scope.Dir, permpolicy.Inputs{}); derr == nil && d.Verdict == permpolicy.VerdictDeny {
			deniedEntry = d.Entry
			env["denied"] = "true"
			env["denied_entry"] = d.Entry
		}
		if deniedEntry != "" {
			env["note"] = fmt.Sprintf("This directory is REFUSED by the operator's permission policy (entry %q). A session grant cannot override it: the deny list outranks a grant. Work in a different directory, or ask the operator to remove the entry from the permission policy file.", deniedEntry)
		} else if scope.FromConversation {
			env["scope"] = askScopeConversation
			env["project_id"] = scope.ProjectID
			env["note"] = "This directory IS this conversation's project: it is the conversation's default scope and PRE-APPROVED, so file/shell work inside it proceeds without asking. Reads never ask anywhere; a write or a command OUTSIDE this directory asks the user first."
		} else {
			env["scope"] = askScopeTenantAnchor
			env["note"] = "This conversation has NO project and NOTHING is pre-approved for it. This directory is only the tenant-wide relative-path anchor, NOT this conversation's own project — every write here is outside this conversation's scope and needs the user's consent. Reads never ask anywhere."
		}
		b, merr := json.Marshal(env)
		if merr != nil {
			return "", merr
		}
		return string(b), nil
	}
	// Host suite: the file/shell tools. The root resolves per execution
	// (fresh project state, never a stale cache) — one DB read, cheap.
	if isHostSuiteTool(name) {
		scope, err := askFileRootResolve(ctx, a.service.pool)
		if err != nil {
			return "", fmt.Errorf("%s: %w", name, err)
		}
		if argsJSON == "" {
			argsJSON = "{}"
		}
		return askHostToolsForRoot(scope.Dir).Execute(ctx, name, argsJSON)
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
