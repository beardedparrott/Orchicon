package askorchicon

import (
	"context"
	"log/slog"
	"strings"
	"testing"
)

// The Ask tool surface has TWO names for every tool and only one of them worked.
//
// BuildSystemPrompt (agent.go, chat.go) teaches the model the MCP-style
// `orchicon_<tool>` form — "named `orchicon_<tool>`" plus one bullet per tool
// rendered as `orchicon_%s` — because that IS the real name on the opencode
// host, where the stdio MCP server registers them. The NATIVE path keys the
// registry by BARE name and sends bare wire definitions, and nothing normalised
// between them:
//
//	ask tool "orchicon_list_projects" is not registered
//
// Every product tool was therefore unreachable through the naming the prompt
// advertised. It looked intermittent because the conversation history carried
// past successful calls as in-context examples; compaction collapsed that
// history, the examples went with it, and the model was left with only the
// prompt's prefixed form — which the dispatcher rejected.
//
// These two tests pin the invariant that was missing: every name the prompt
// advertises must dispatch, and the bare names must keep working.
func TestEveryAdvertisedAskToolNameResolves(t *testing.T) {
	r := NewToolRegistry(nil, slog.Default(), nil)
	if len(r.List()) == 0 {
		t.Fatal("fixture: the registry must not be empty")
	}
	for _, d := range r.List() {
		if strings.HasPrefix(d.Name, askToolNamePrefix) {
			t.Errorf("registry tool %q already carries the %q prefix — normalisation would be ambiguous", d.Name, askToolNamePrefix)
			continue
		}
		advertised := askToolNamePrefix + d.Name
		// The form the PROMPT teaches must resolve to this tool.
		got := normalizeAskToolName(advertised)
		if got != d.Name {
			t.Errorf("normalizeAskToolName(%q) = %q, want the registry key %q", advertised, got, d.Name)
		}
		if _, ok := r.Get(got); !ok {
			t.Errorf("tool %q is advertised to the model as %q but does not resolve — the model cannot call it", d.Name, advertised)
		}
		// The BARE form the wire definitions use must keep working too.
		if _, ok := r.Get(d.Name); !ok {
			t.Errorf("tool %q does not resolve by its bare (wire) name", d.Name)
		}
	}
}

// The end-to-end pin: a prefixed call must actually DISPATCH, not merely resolve.
// list_adapter_kinds ignores both the pool and the args, so this exercises the
// real lookup path with no database.
func TestExecuteAskToolAcceptsTheAdvertisedPrefix(t *testing.T) {
	s := &Service{toolRegistry: NewToolRegistry(nil, slog.Default(), nil)}
	s.registerSessionTools()
	a := &nativeAskTools{service: s}

	out, err := a.ExecuteAskTool(context.Background(), askToolNamePrefix+"list_adapter_kinds", "{}")
	if err != nil {
		if strings.Contains(err.Error(), "not registered") {
			t.Fatalf("a prefixed call must dispatch — this is the bug that bricked the Ask tool surface: %v", err)
		}
		t.Fatalf("prefixed ExecuteAskTool: %v", err)
	}
	if !strings.Contains(out, "adapter_kinds") {
		t.Fatalf("out = %q, want the adapter-kinds payload", out)
	}

	// And the bare form still works.
	if _, err := a.ExecuteAskTool(context.Background(), "list_adapter_kinds", "{}"); err != nil {
		t.Fatalf("bare ExecuteAskTool: %v", err)
	}

	// A genuinely unknown name still fails LOUD (normalisation must not turn the
	// registry into a permissive match-anything).
	if _, err := a.ExecuteAskTool(context.Background(), askToolNamePrefix+"no_such_tool_exists", "{}"); err == nil || !strings.Contains(err.Error(), "not registered") {
		t.Fatalf("unknown tool error = %v, want a loud 'not registered'", err)
	}
}
