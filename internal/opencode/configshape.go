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
	// ConfigShapeV1 is the ZERO VALUE ON PURPOSE.
	ConfigShapeV1 ConfigShape = ""
	// ConfigShapeV2 is the native 2.x document shape.
	ConfigShapeV2 ConfigShape = "v2"
)

func (s ConfigShape) isV2() bool { return s == ConfigShapeV2 }

// configShapeResolver is the seam the config builders consult.
var configShapeResolver = configShapeForHost

// SetConfigShapeResolver overrides how the config document's shape is decided.
func SetConfigShapeResolver(fn func(log *slog.Logger) ConfigShape) {
	if fn == nil {
		configShapeResolver = configShapeForHost
		return
	}
	configShapeResolver = fn
}

// configShapeForHost derives the shape from the opencode binary THIS HOST resolves.
func configShapeForHost(log *slog.Logger) ConfigShape {
	binary, err := resolveOpenCodeBinary()
	if err != nil || strings.TrimSpace(binary) == "" {
		return ConfigShapeV1
	}
	return ConfigShapeForBinary(context.Background(), log, binary)
}

// ConfigShapeForBinary reports the config shape required by the opencode binary at path.
func ConfigShapeForBinary(ctx context.Context, log *slog.Logger, binary string) ConfigShape {
	if shapeForBinary(ctx, log, binary).name == shapeV2.name {
		return ConfigShapeV2
	}
	return ConfigShapeV1
}

// v2PermissionAction maps a V1 permission KEY to the V2 ACTION name.
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

// v2PermissionRules renders the V1 permission MAP (tool -> {pattern: effect}) as the V2 ordered
// RULE ARRAY, preserving what the map MEANT.
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
		// An unmapped action is carried through unchanged rather than dropped: dropping it would silently
		// widen access, and the drift test (TestV2PermissionActionsAreKnown) is what surfaces the gap.
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

// v2MCPBlock wraps the flat V1 `mcp` map in the V2 `mcp.servers` object and renames the two entry
// fields that changed.
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

// v2ProviderBlock renames the V1 provider fields: "The singular provider map becomes providers.
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

// aisdkPackage qualifies a provider package for V2.
func aisdkPackage(pkg string) string {
	if strings.Contains(pkg, ":") {
		return pkg
	}
	if strings.HasPrefix(pkg, "@ai-sdk/") {
		return "aisdk:" + pkg
	}
	return pkg
}

// v2AgentEntry renders one agent definition in the V2 shape: the prompt field is `system` ("prompt
// becomes system"), and a system value "replaces the provider's base prompt for that agent".
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
