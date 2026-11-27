package antigravity

// permissions.go — THE WORKER PERMISSION POLICY, answered without a human.
//
// agy asks `session/request_permission` before a gated tool call and blocks until the client answers
// (recorded: tool_run_command_permission_allow.jsonl:20 asks, the client answers with the `allow`
// optionId, the command runs). A worker execution has NO human attached, so every request must be
// answered from the platform's own policy — never by asking the operator, and never by launching agy
// with the permission system switched off.
//
// THIS FILE IS A TRANSLATION LAYER, NOT A SECOND POLICY. The rule set that decides what a worker may
// do already exists and is already trusted: internal/claude's DecideTool runs the never-allow class
// (workerrestrict.MatchCommand → internal/neverallow), the protected-path roots
// (internal/protectedpath), the operator's permpolicy deny list and the project/carve-out boundary
// (workerrestrict.OutsideProject) for the claude worker profile. Re-implementing any of that here
// would create a second copy that can drift, and the two adapters would then disagree about whether
// `rm -rf /` is allowed. So this file:
//
//   1. normalises an agy tool call into claude's tool vocabulary (run_command → Bash with
//      input["command"], write_file → Write with input["file_path"], …),
//   2. adds the ONE agy-specific rule (its built-in subagent tools, which claude spells
//      Task/Agent), and
//   3. calls claude.DecideTool for everything else.
//
// FAIL CLOSED. Every abnormal path — a nil policy, a panic, an unreadable policy file, a request agy
// offered no reject option for — answers REJECT. The acp package already guarantees the wire half of
// this (conn.go's failClosed); this file guarantees the decision half. There is no branch here that
// answers "allow" for a call the policy did not allow.

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/beardedparrott/orchicon/internal/antigravity/acp"
	"github.com/beardedparrott/orchicon/internal/claude"
	"github.com/beardedparrott/orchicon/internal/permpolicy"
	"github.com/beardedparrott/orchicon/internal/workerrestrict"
)

// PolicyHandler implements acp.Handler for a WORKER execution: it answers every permission request
// from the platform's rule set, with no human in the loop.
type PolicyHandler struct {
	// ProjectDir is the project root — the boundary an external-directory access may not leave.
	ProjectDir string
	// WorktreeDir is the run's worktree when one is provisioned: the session's cwd, and therefore the
	// directory a RELATIVE tool path resolves against. workerrestrict.OutsideProject already treats any
	// path below ProjectDir as in scope, and a worktree is a subdirectory of it, so adding it here
	// widens nothing; it is used to resolve relative paths for the log line and the backstop.
	WorktreeDir string
	// PolicyPath is the operator's permission policy file. Empty disables the policy rung (never the
	// never-allow class).
	PolicyPath string
	// Log receives every decision. Nil is fine.
	Log *slog.Logger
	// OnViolation, when set, is called for every DENIED call with a human-readable reason. The session
	// wires it to a durable error part, because agy gets no reason over ACP and the refusal would
	// otherwise be invisible to whoever reads the execution afterwards.
	OnViolation func(tool, reason string)
}

// policyHandlerLog returns a usable logger.
func (h *PolicyHandler) logger() *slog.Logger {
	if h == nil || h.Log == nil {
		return slog.Default()
	}
	return h.Log
}

// boundaryDir is the directory the project boundary is judged against: the worktree when one exists,
// else the project root. It is the same rule claude's PermissionOptions applies (WorktreeDir widens,
// ProjectDir bounds).
func (h *PolicyHandler) boundaryDir() string {
	if strings.TrimSpace(h.WorktreeDir) != "" {
		return h.WorktreeDir
	}
	return h.ProjectDir
}

// RequestPermission implements acp.Handler.
//
// It NEVER returns an error: an error makes the client answer with the reject option, which is what a
// decision of ours would produce anyway, but returning a decision keeps the reason available for the
// log and the OnViolation record. A panic is recovered into a reject for the same reason — the wire
// fails closed either way, and a recovered panic does not take the reader goroutine's request path
// down with it.
func (h *PolicyHandler) RequestPermission(ctx context.Context, req acp.PermissionRequest) (decision acp.PermissionDecision, err error) {
	defer func() {
		if r := recover(); r != nil {
			h.logger().Error("antigravity permission policy panicked — answering REJECT (fail closed)",
				"tool", req.ToolCall.Name, "panic", fmt.Sprint(r))
			decision = h.reject(req.Options)
			err = nil
		}
	}()

	verdict := h.Decide(req.ToolCall)
	if verdict.Allow {
		allowID, ok := acp.AllowOptionID(req.Options)
		if !ok {
			// agy offered no allow at all. Answering `cancelled` does not grant the call, which is the
			// only safe reading of "the agent did not offer the outcome we wanted".
			h.logger().Warn("antigravity permission: policy allows this call but agy offered no allow option — answering cancelled",
				"tool", req.ToolCall.Name)
			return acp.PermissionDecision{Cancelled: true}, nil
		}
		h.logger().Debug("antigravity permission: allowed", "tool", req.ToolCall.Name, "rule", verdict.Rule)
		return acp.PermissionDecision{OptionID: allowID}, nil
	}

	reason := verdict.Reason
	if reason == "" {
		reason = "refused by the Orchicon worker restriction"
	}
	h.logger().Warn("antigravity permission: REFUSED",
		"tool", req.ToolCall.Name, "rule", verdict.Rule, "reason", reason)
	if h.OnViolation != nil {
		h.OnViolation(req.ToolCall.Name, reason)
	}
	return h.reject(req.Options), nil
}

// reject picks the reject answer, or `cancelled` when agy offered no reject option.
func (h *PolicyHandler) reject(opts []acp.PermissionOption) acp.PermissionDecision {
	if id, ok := acp.RejectOptionID(opts); ok {
		return acp.PermissionDecision{OptionID: id}
	}
	return acp.PermissionDecision{Cancelled: true}
}

// Decide runs the worker rule set for one agy tool call. It is exported so the decision can be
// table-tested without a wire, which is exactly how AC2 is proved.
//
// `allow_once` is the ONLY approving answer: agy offers `allow_once`/`reject_once` today, and an
// always-allow option would outlive the single call the policy actually judged.
func (h *PolicyHandler) Decide(call acp.ToolCallInfo) claude.HookVerdict {
	name := strings.TrimSpace(call.Name)

	// THE AGY-SPECIFIC RULE, FIRST. agy's subagent tools are the direct analogue of claude's
	// Task/Agent, which claude.DecideTool refuses for every worker; agy names them differently so the
	// claude rule cannot see them.
	if workerrestrict.IsAntigravitySubagentTool(name) {
		return claude.HookVerdict{
			Allow: false,
			Rule:  workerrestrict.TaskToolDeny,
			Reason: "refused: agy's built-in subagent tool is denied for every worker execution — Orchicon " +
				"already splits the work into focused steps, and a spawned subagent re-carries the parent's context.",
		}
	}

	hook := claude.HookInput{
		ToolName:  hookToolName(name),
		ToolInput: hookToolInput(name, call),
	}
	// The boundary is the WORKTREE when the run has one (that is the session's cwd and the thing a
	// relative path resolves against), else the project root. Carve-outs are derived inside
	// workerrestrict, so /tmp/orchicon and .orchicon/ stay usable exactly as they do for claude.
	verdict := claude.DecideTool(hook, h.boundaryDir(), h.PolicyPath)
	if !verdict.Allow {
		return verdict
	}
	// An agy edit whose target sits outside the boundary is refused even though the name alone looked
	// fine: claude's path rule only fires for the names it knows, and agy's edit tool names vary.
	if p := toolInputPath(hook.ToolInput); p != "" {
		if allowed, rule := workerrestrict.OutsideProject(p, h.boundaryDir()); !allowed {
			return claude.HookVerdict{
				Allow: false,
				Rule:  rule,
				Reason: fmt.Sprintf("refused: %s is outside the project and outside every carve-out (%s and "+
					".orchicon run metadata) — the same external-directory boundary the claude worker profile "+
					"enforces.", p, workerrestrict.ScratchDir),
			}
		}
	}
	return verdict
}

// hookToolName normalises one agy tool name into claude's tool vocabulary, which is what DecideTool
// rules on. A name with no claude equivalent keeps its own spelling: DecideTool allows an unrecognised
// name (MCP tools and agy's own `ask_question`), which is the same parity rule the native bridge
// applies to a discovered MCP tool.
func hookToolName(name string) string {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "run_command", "bash":
		return "Bash"
	case "write_file", "write_to_file", "create_file", "write":
		return "Write"
	case "edit_file", "replace_file_content", "multi_replace_file_content", "apply_patch", "edit":
		return "Edit"
	case "read_file", "view_file", "open_file", "read":
		return "Read"
	case "grep_search", "grep":
		return "Grep"
	case "list_dir", "list_directory":
		return "LS"
	case "find_by_name", "glob":
		return "Glob"
	case "search_web", "web_search":
		return "WebSearch"
	case "read_url_content", "web_fetch":
		return "WebFetch"
	}
	return name
}

// hookToolInput translates an agy tool call's arguments into claude's tool_input keys, so the shared
// rule set reads the command and the path it knows (`command`, `file_path`).
func hookToolInput(name string, call acp.ToolCallInfo) map[string]any {
	in := agyInput(call.RawInput, call.Args)
	out := make(map[string]any, len(in)+2)
	for k, v := range in {
		out[k] = v
	}
	if cmd := toolInputCommand(in); cmd != "" {
		out["command"] = cmd
	}
	if p := toolInputPath(in); p != "" {
		out["file_path"] = p
	} else if len(call.Locations) > 0 && strings.TrimSpace(call.Locations[0].Path) != "" {
		// THE LOCATION IS A HINT, NOT A SUBSTITUTE: a call that named no path in its args but carries a
		// location is still a path-touching call, and the boundary rule must see it.
		out["file_path"] = strings.TrimSpace(call.Locations[0].Path)
	}
	return out
}

// DefaultPolicyPath is the operator's policy file for this plane. It exists as a function so the
// session reads it at the same moment claude does (permpolicy.DefaultPath honours the env override).
func DefaultPolicyPath() string { return permpolicy.DefaultPath() }

// marshalToolInput renders a tool input for a log line or a durable part.
func marshalToolInput(in map[string]any) []byte {
	if in == nil {
		return nil
	}
	b, err := json.Marshal(in)
	if err != nil {
		return nil
	}
	return b
}
