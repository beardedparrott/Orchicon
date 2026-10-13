package runtime

import "testing"

// TestAgentModelForServeStripsTheAdapterSegment pins the conversion every other
// consumer of a model ref already performs, and which the per-worker agent initially did
// not.
//
// A worker's model_ref carries the control plane's ADAPTER segment
// (opencode/opencode/<model>), while an opencode config's model field takes
// provider/model. Passing the raw ref put a three-segment string where two were
// expected — the same boundary every other call site crosses with adapter.SplitForServe.
func TestAgentModelForServeStripsTheAdapterSegment(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"opencode/opencode/muse-spark-1.3-contributor-free", "opencode/muse-spark-1.3-contributor-free"},
		{"opencode/ollama-cloud/deepseek-v4.1-flash", "ollama-cloud/deepseek-v4.1-flash"},
		// A model id may itself contain slashes — provider/model/generation — and the
		// remainder is preserved verbatim (ADR-0003).
		{"opencode/commandcode/deepseek/deepseek-v4-flash-fast", "commandcode/deepseek/deepseek-v4-flash-fast"},
		// A legacy 2-segment ref already IS provider/model.
		{"opencode-go/deepseek-v4-flash", "opencode-go/deepseek-v4-flash"},
	} {
		if got := agentModelForServe(c.in); got != c.want {
			t.Errorf("agentModelForServe(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestAgentModelForServeOmitsWhatItCannotRender pins the safe failure: an unusable ref
// yields NO model rather than a malformed one. A session's model is set on the SESSION,
// so an agent with none inherits it — whereas a malformed model is a value opencode must
// reject, and the v2 schema pattern-validates the field.
func TestAgentModelForServeOmitsWhatItCannotRender(t *testing.T) {
	for _, in := range []string{"", "   ", "justamodel", "opencode/", "/model"} {
		if got := agentModelForServe(in); got != "" {
			t.Errorf("agentModelForServe(%q) = %q, want \"\" — a model we cannot render must be OMITTED, never guessed", in, got)
		}
	}
}
