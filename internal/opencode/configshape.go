package opencode

import (
	"context"
	"log/slog"
	"sort"
	"strings"
)

// configshape.go — WHICH SHAPE the OPENCODE_CONFIG_CONTENT document must be
// written in, decided by the same version probe the API surface uses.
//
// opencode 2 renamed a large part of the config vocabulary. The migration guide
// states that existing V1 configuration "is intended to keep working" through an
// in-memory normalizer, and most of it does — but at least one field is on the
// EXPLICIT IGNORE list ("Agent name inside V1 JSON configuration"), and the
// ignore list's own framing is the problem: a dropped field is a WARNING, not an
// error, so the failure mode of relying on normalization is a config that
// silently means something else. The two that bit us:
//
//	agent.<name>.prompt  -> must be agents.<name>.system, or the agent is never
//	                        registered, `default_agent` falls back to opencode's
//	                        built-in `build` agent, and a worker runs under
//	                        opencode's large default prompt AND ours.
//	permission           -> must be permissions, or the worker sandbox is not in
//	                        force. That one fails SILENTLY and LOUDLY in the wrong
//	                        direction: broader tool access than intended.
//
// So the document is written in the shape the running binary understands, and
// nothing here is guessed: every mapping below is quoted from the V2 reference
// (agents, permissions, providers, and the migrate-from-V1 table).
//
// WHAT IS DELIBERATELY NOT DONE. This is a SHAPE translation, not a redesign: the
// rules, servers, providers and prompts are the ones Orchicon already builds. The
// alternative — teaching every producer about two vocabularies — is how the two
// drift, which is the same reasoning apisurface.go gives for owning the routes.

// ConfigShape is the shape of the injected config document.
type ConfigShape string

const (
	// ConfigShapeV1 is the ZERO VALUE ON PURPOSE. Every caller that predates the
	// v2 work leaves ConfigOptions.Shape unset and therefore emits the v1
	// document byte-identically — a 1.x installation must not change behaviour
	// because we learned about 2.x. A test pins that: see
	// TestV1ConfigIsByteIdentical.
	ConfigShapeV1 ConfigShape = ""
	// ConfigShapeV2 is the native 2.x document shape.
	ConfigShapeV2 ConfigShape = "v2"
)

func (s ConfigShape) isV2() bool { return s == ConfigShapeV2 }

// configShapeResolver is the seam the config builders consult. It is a variable
// rather than a direct call so the value is INJECTABLE: the shape is a property
// of the opencode binary a machine happens to have installed, and a test that
// asserted the document without controlling it would pass or fail depending on
// the developer's opencode version. Production never changes it; the package's
// TestMain pins it, and the v2 tests set it explicitly.
var configShapeResolver = configShapeForHost

// SetConfigShapeResolver overrides how the config document's shape is decided.
// Passing nil restores the real host probe, so a test cannot leave the parent
// process pointed at a stub (the same contract SetBinaryResolver keeps).
func SetConfigShapeResolver(fn func(log *slog.Logger) ConfigShape) {
	if fn == nil {
		configShapeResolver = configShapeForHost
		return
	}
	configShapeResolver = fn
}

// configShapeForHost derives the shape from the opencode binary THIS HOST
// resolves. The runtime container's serve runs the SAME install bind-mounted
// into the container, so the host's answer is the container's answer, and the
// detection is process-cached (shapeForBinary) — it costs one subprocess for the
// life of the process, not one per run.
//
// An unresolvable binary or a failed detection yields V1, which is
// shapeForBinary's own documented default: "v1 is what the installed base runs,
// so a version we cannot read must leave behaviour exactly as it was."
func configShapeForHost(log *slog.Logger) ConfigShape {
	binary, err := resolveOpenCodeBinary()
	if err != nil || strings.TrimSpace(binary) == "" {
		return ConfigShapeV1
	}
	return ConfigShapeForBinary(context.Background(), log, binary)
}

// ConfigShapeForBinary reports the config shape required by the opencode binary
// at path. Exported so a caller holding an already-resolved binary does not
// re-derive the rule.
func ConfigShapeForBinary(ctx context.Context, log *slog.Logger, binary string) ConfigShape {
	if shapeForBinary(ctx, log, binary).name == shapeV2.name {
		return ConfigShapeV2
	}
	return ConfigShapeV1
}

// v2PermissionAction maps a V1 permission KEY to the V2 ACTION name. The V2
// reference states it directly: "In V2, use permissions, shell, and subagent
// instead of permission, bash, and task", and the Tools reference gives each
// tool's action ("write ... Permission: edit", "patch ... Permission: edit").
//
// The mapping is NOT injective, which is why the translation dedupes: V1 `write`
// and V1 `edit` are both the single V2 action `edit`. Nothing here is
// speculative — an action we have not been told about is NOT invented (see
// v2PermissionRules), because an unknown action is a rule that never matches and
// would quietly drop a restriction.
var v2PermissionAction = map[string]string{
	"bash":               "shell",
	"task":               "subagent",
	"write":              "edit",
	"edit":               "edit",
	"read":               "read",
	"grep":               "grep",
	"glob":               "glob",
	"external_directory": "external_directory",
}

// v2PermissionRules renders the V1 permission MAP (tool -> {pattern: effect}) as
// the V2 ordered RULE ARRAY, preserving what the map MEANT.
//
// ORDER IS THE SEMANTICS, and the two formats disagree about it. V1 is a map:
// unordered, and opencode resolved it by its own priority. V2 is an ordered list
// where "the last matching rule wins" and "agent rules are appended last". Every
// map Orchicon builds states its broad rule as "*" and its exceptions as specific
// patterns, so the translation emits, per action, the "*" rule FIRST and the
// specific patterns after it — which is the same intent expressed in the new
// order. Patterns are SORTED (never map order) so the document is deterministic:
// it feeds the warm-pool key, and a document that reordered itself would churn
// containers and make every assertion flaky.
func v2PermissionRules(profile PermissionProfile, compositeTools bool) []any {
	v1 := permissionRules(profile, compositeTools)

	actions := make([]string, 0, len(v1))
	for action := range v1 {
		actions = append(actions, action)
	}
	sort.Strings(actions)

	out := make([]any, 0, len(v1))
	seen := make(map[string]struct{}, len(v1))
	for _, v1Action := range actions {
		patterns, ok := v1[v1Action].(map[string]any)
		if !ok {
			continue
		}
		// An unmapped action is carried through unchanged rather than dropped:
		// dropping it would silently widen access, and the drift test
		// (TestV2PermissionActionsAreKnown) is what surfaces the gap.
		action, mapped := v2PermissionAction[v1Action]
		if !mapped {
			action = v1Action
		}

		keys := make([]string, 0, len(patterns))
		for p := range patterns {
			keys = append(keys, p)
		}
		sort.Strings(keys)
		ordered := make([]string, 0, len(keys))
		for _, k := range keys {
			if k == "*" {
				ordered = append(ordered, k)
			}
		}
		for _, k := range keys {
			if k != "*" {
				ordered = append(ordered, k)
			}
		}

		for _, resource := range ordered {
			effect, _ := patterns[resource].(string)
			key := action + "\x00" + resource
			if _, dup := seen[key]; dup {
				continue
			}
			seen[key] = struct{}{}
			out = append(out, map[string]any{
				"action":   action,
				"resource": resource,
				"effect":   effect,
			})
		}
	}
	return out
}

// v2MCPBlock wraps the flat V1 `mcp` map in the V2 `mcp.servers` object and
// renames the two entry fields that changed.
//
// Per the migrate-from-V1 table: "V2 groups servers under mcp.servers, replaces
// enabled with the inverse disabled". The split of `timeout` by purpose
// ("timeout: { catalog, execution }") is applied when a single number is
// present, giving the V1 value to both purposes — which is what one number
// meant.
func v2MCPBlock(mcp map[string]any) map[string]any {
	servers := make(map[string]any, len(mcp))
	for name, raw := range mcp {
		entry, ok := raw.(map[string]any)
		if !ok {
			servers[name] = raw
			continue
		}
		v2 := make(map[string]any, len(entry))
		for k, v := range entry {
			switch k {
			case "enabled":
				if b, isBool := v.(bool); isBool {
					v2["disabled"] = !b
					continue
				}
				v2[k] = v
			case "timeout":
				if n, isNum := v.(int); isNum {
					v2["timeout"] = map[string]any{"catalog": n, "execution": n}
					continue
				}
				v2[k] = v
			default:
				v2[k] = v
			}
		}
		servers[name] = v2
	}
	return map[string]any{"servers": servers}
}

// v2ProviderBlock renames the V1 provider fields: "The singular provider map
// becomes providers. V1 npm becomes package, and AI SDK packages receive the
// aisdk: prefix. api becomes settings.baseURL."
//
// The aisdk: prefix is applied to AI SDK packages only, and those are the ones
// Orchicon injects (an OpenAI-compatible local server is @ai-sdk/*), so the
// prefix is derived from the package name rather than assumed: a package that is
// not an AI SDK one is passed through untouched, which is what the reference
// describes and what a future non-SDK provider needs.
func v2ProviderBlock(providers map[string]any) map[string]any {
	out := make(map[string]any, len(providers))
	for id, raw := range providers {
		entry, ok := raw.(map[string]any)
		if !ok {
			out[id] = raw
			continue
		}
		v2 := make(map[string]any, len(entry))
		if pkg, isStr := entry["npm"].(string); isStr && pkg != "" {
			v2["package"] = aisdkPackage(pkg)
		}
		if opts, isMap := entry["options"].(map[string]any); isMap && len(opts) > 0 {
			settings := make(map[string]any, len(opts))
			for k, v := range opts {
				settings[k] = v
			}
			v2["settings"] = settings
		}
		out[id] = v2
	}
	return out
}

// aisdkPackage qualifies a provider package for V2. "AI SDK packages receive the
// aisdk: prefix" — so an @ai-sdk/* package is qualified, one already carrying a
// scheme (aisdk:, npm:, or similar) is left alone, and anything else is passed
// through as-is rather than being mangled into a scope it does not have.
func aisdkPackage(pkg string) string {
	if strings.Contains(pkg, ":") {
		return pkg
	}
	if strings.HasPrefix(pkg, "@ai-sdk/") {
		return "aisdk:" + pkg
	}
	return pkg
}

// v2AgentEntry renders one agent definition in the V2 shape: the prompt field is
// `system` ("prompt becomes system"), and a system value "replaces the provider's
// base prompt for that agent" — which is exactly the behaviour the V1 `prompt`
// field produced, and the reason registering the agent in the right shape is what
// stops `build`'s prompt being used.
//
// The model is OMITTED when empty rather than emitted as "". The V2 schema types
// it as a reference with the pattern `^[^/#]+/[^#]+(?:#[^#]+)?$`, so an empty
// string is a validation failure, where V1 simply ignored it.
func v2AgentEntry(prompt, modelRef string) map[string]any {
	entry := map[string]any{"mode": "primary"}
	if strings.TrimSpace(prompt) != "" {
		entry["system"] = prompt
	}
	if strings.TrimSpace(modelRef) != "" {
		entry["model"] = modelRef
	}
	return entry
}
