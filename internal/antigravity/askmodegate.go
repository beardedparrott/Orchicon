package antigravity

// askmodegate.go — THE ASK MODE BOUNDARY, enforced at the ACP permission handler.
//
// WHY THE GATE LIVES HERE. The claude Ask transport gets its mode boundary through a per-conversation mode
// FILE that its `PermissionRequest` hook reads (internal/claude/modegate.go). agy has no hook: the ONE thing
// it asks the client before a gated tool call is `session/request_permission`, and `agy acp` on 1.5.13 has
// NO `--settings` flag (checked with `agy acp --help`), so there is no config document to hand it either. The
// permission handler is therefore the ONLY place the boundary can hold — and it holds it BEFORE any card is
// raised, so a refused tool is never put in front of the operator as a question they may answer "yes" to.
//
// THE TOOL TABLES ARE NOT DUPLICATED. `internal/askmode` owns the policy (which names each mode denies, why,
// and which mode does the work instead), and `claude.AskModeRefusal` owns the wording. This file's only job
// is the TRANSLATION between agy's tool vocabulary and the policy's, which is the same shape claude's
// claudeToolToPolicyName has — per-adapter, because the vocabularies are.

import (
	"crypto/sha256"
	"fmt"
	"sort"
	"strings"

	"github.com/beardedparrott/orchicon/internal/askmode"
	"github.com/beardedparrott/orchicon/internal/claude"
)

// askModeGate reports whether the turn's mode refuses this tool call, and the refusal text to hand back.
//
// EVERY ANSWER HERE IS ABOUT THE MODE, never about consent: `false, ""` means "the mode permits it, decide
// the card elsewhere". The order of the three classification arms is deliberate and is the whole correctness
// argument:
//
//  1. A CANONICAL platform tool (`bash`/`write`/`edit`/…): the policy speaks those names directly.
//  2. THE PLATFORM'S OWN MCP SIDE-CAR (`mcp_orchicon_<tool>`, `mcp__orchicon__<tool>`): the policy speaks
//     the bare registry name (`create_work_item`), so the prefix is stripped. Without this arm
//     `mcp_orchicon_orchicon_bash` would read as an opaque MCP tool and slip past Brainstorm's `bash` denial
//     — a prefix would be a way around the gate.
//  3. ANY OTHER MCP TOOL: opaque. The mode may run it only if the mode MAY ACT (askmode.Policy.MayAct), which
//     is what stops a planning conversation from dispatching real work through a third party's server.
//
// Everything else (`view_file`→`read`, `grep_search`→`grep`, `ask_question`, an unknown tool) is allowed:
// no mode denies reading, and a tool the policy does not name is not a policy decision.
func askModeGate(mode, rawName, canonical string) (bool, string) {
	if strings.TrimSpace(mode) == "" {
		// NO MODE STAMPED: the policy's own answer for an unknown mode is ALLOW (askmode.Allows), and this
		// gate must not invent a stricter boundary than the platform's.
		return false, ""
	}
	policyName := askPolicyNameFor(rawName, canonical)
	if policyName == "" {
		// An opaque MCP tool: the may-act rule decides, and it is the SAME table the other adapters read.
		if askmode.MayAct(mode) {
			return false, ""
		}
		return true, claude.AskModeRefusal(rawName, mode)
	}
	if askmode.Allows(mode, policyName) {
		return false, ""
	}
	return true, claude.AskModeRefusal(rawName, mode)
}

// askPolicyNameFor maps an agy tool call onto the policy's tool vocabulary.
//
// "" means OPAQUE — an MCP tool the platform cannot classify, which askmode decides through MayAct rather
// than through a name.
func askPolicyNameFor(rawName, canonical string) string {
	name := strings.TrimSpace(rawName)
	if isMCPTool(name) {
		if isOrchiconMCPTool(name) {
			// The platform's OWN server: strip the transport prefix and hand the policy the bare registry
			// name. `mcp_orchicon_orchicon_bash` → `orchicon_bash` → (askmode.NormalizeToolName) → `bash`.
			//
			// BOTH SPELLINGS, because isOrchiconMCPTool accepts both and a gate that handled only agy's would
			// wave claude's spelling straight through.
			lower := strings.ToLower(name)
			switch {
			case strings.HasPrefix(lower, "mcp__orchicon__"):
				return askmode.NormalizeToolName(name[len("mcp__orchicon__"):])
			case strings.HasPrefix(lower, "mcp_orchicon_"):
				return askmode.NormalizeToolName(name[len("mcp_orchicon_"):])
			}
			return askmode.NormalizeToolName(name)
		}
		// AN OPERATOR'S MCP SERVER: opaque, by construction.
		return ""
	}
	if c := strings.TrimSpace(canonical); c != "" {
		return c
	}
	return strings.TrimSpace(rawName)
}

// askMCPFingerprint is "what this child was launched with", comparable across turns: whether the mode could
// act (which decides whether the operator's servers were OFFERED at all) and a hash of the rendered servers.
//
// IT HASHES THE RENDERED SERVERS, NOT THE RESOLUTION. The raw resolution still carries `${SECRET_NAME}`
// references — they are expanded during render — so fingerprinting it could not see a rotated secret. The
// child's `mcpServers` list is fixed at fork/exec, so the only way to apply a rotation is to re-attach, and
// this is what decides whether to.
//
// HASHED, NOT STORED, for the same reason claude's is: the value travels in a log line, and a resolved
// credential must never be in one.
func askMCPFingerprint(mayAct bool, servers []acpMCPServerKey) string {
	h := sha256.New()
	for _, s := range servers {
		_, _ = fmt.Fprintf(h, "name=%q\ncmd=%q\n", s.Name, s.Command)
		for _, a := range s.Args {
			_, _ = fmt.Fprintf(h, "arg=%q\n", a)
		}
		_, _ = fmt.Fprintf(h, "url=%q\n", s.URL)
		vars := append([]string(nil), s.Env...)
		sort.Strings(vars)
		for _, kv := range vars {
			_, _ = fmt.Fprintf(h, "env=%q\x00", kv)
		}
		vars = append([]string(nil), s.Headers...)
		sort.Strings(vars)
		for _, kv := range vars {
			_, _ = fmt.Fprintf(h, "hdr=%q\x00", kv)
		}
	}
	return fmt.Sprintf("mayact=%v;%x", mayAct, h.Sum(nil))
}

// withholdOpaqueACPMCP drops every server from an Ask render EXCEPT the platform's own Orchicon sidecar.
//
// THIS IS THE "OFFERED" HALF of the mode boundary, mirroring claude's withholdOpaqueMCP: a mode that may not
// act is not even shown the operator's servers, so the model cannot call a tool whose result would have to be
// refused. The permission handler's gate is the ENFORCED half; this is what keeps the two agreeing.
func withholdOpaqueACPMCP(servers []acpMCPServerKey) []acpMCPServerKey {
	out := make([]acpMCPServerKey, 0, 1)
	for _, s := range servers {
		if strings.EqualFold(strings.TrimSpace(s.Name), askOrchiconServerName) {
			out = append(out, s)
		}
	}
	return out
}

// askOrchiconServerName is the name the platform's own sidecar is registered under. It matches
// claude.OrchiconMCPServer's Name (`orchicon`) and askmode's own `orchicon` server segment, because all three
// describe one sidecar and a divergent spelling here would make the withdraw rule drop the wrong entries.
const askOrchiconServerName = "orchicon"

// acpMCPServerKey is the comparable view of an ACP server entry the fingerprint and the withhold rule read.
// It is a separate type rather than acp.MCPServer so the ordering is explicit: a fingerprint over a wire type
// would change meaning the day the wire type gains a field.
type acpMCPServerKey struct {
	Name    string
	Command string
	Args    []string
	URL     string
	Env     []string
	Headers []string
}
