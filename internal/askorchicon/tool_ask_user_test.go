package askorchicon

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/beardedparrott/orchicon/internal/askmode"
	"github.com/beardedparrott/orchicon/internal/db"
)

// TestParseAskUserArgs is the validation table: the loud refusals the AC
// requires (at least two options, or the explicit free-text-only form) plus the
// accepted shapes.
func TestParseAskUserArgs(t *testing.T) {
	cases := []struct {
		name       string
		args       string
		wantErrSub string // "" = accept
		wantOpts   int
	}{
		{
			name:     "two string options",
			args:     `{"question":"Which branch?","options":["develop","main"]}`,
			wantOpts: 2,
		},
		{
			name:     "three object options",
			args:     `{"question":"How should I proceed?","options":[{"label":"A","description":"first"},{"label":"B"},{"label":"C"}]}`,
			wantOpts: 3,
		},
		{
			name:     "mixed string and object options",
			args:     `{"question":"Pick","options":["plain",{"label":"object","description":"d"}]}`,
			wantOpts: 2,
		},
		{
			name:     "free-text only form",
			args:     `{"question":"What should the title be?","options":[],"allow_other":true}`,
			wantOpts: 0,
		},
		{
			name:       "zero options without allow_other is refused",
			args:       `{"question":"Pick","options":[]}`,
			wantErrSub: "provide at least two options, or set allow_other=true",
		},
		{
			name:       "a single option is refused",
			args:       `{"question":"Pick","options":["only one"]}`,
			wantErrSub: "provide at least two options, or set allow_other=true",
		},
		{
			name:       "a single option is refused even with allow_other",
			args:       `{"question":"Pick","options":["only one"],"allow_other":true}`,
			wantErrSub: "provide at least two options, or set allow_other=true",
		},
		{
			name:       "blank question is refused",
			args:       `{"question":"   ","options":["a","b"]}`,
			wantErrSub: "`question` is required",
		},
		{
			name:       "blank option label is refused",
			args:       `{"question":"Pick","options":["a",""]}`,
			wantErrSub: "empty label",
		},
		{
			name:       "blank object option label is refused",
			args:       `{"question":"Pick","options":[{"label":"a"},{"label":"  "}]}`,
			wantErrSub: "empty label",
		},
		{
			name:       "options that are not an array are refused",
			args:       `{"question":"Pick","options":{"label":"a"}}`,
			wantErrSub: "not a valid JSON object",
		},
		{
			name:       "empty arguments are refused",
			args:       ``,
			wantErrSub: "missing arguments",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			q, opts, _, err := parseAskUserArgs([]byte(tc.args))
			if tc.wantErrSub != "" {
				if err == nil {
					t.Fatalf("expected an error containing %q, got nil (opts=%v)", tc.wantErrSub, opts)
				}
				if !strings.Contains(err.Error(), tc.wantErrSub) {
					t.Fatalf("error = %q, want it to name the fix %q", err.Error(), tc.wantErrSub)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if q == "" {
				t.Error("accepted args must yield a non-empty question")
			}
			if len(opts) != tc.wantOpts {
				t.Errorf("options = %d (%v), want %d", len(opts), opts, tc.wantOpts)
			}
			for i, o := range opts {
				if strings.TrimSpace(o.Label) == "" {
					t.Errorf("option %d has an empty label after normalisation", i)
				}
			}
		})
	}
}

// TestToolAskUserReturnsWithoutWaitingOnContext proves the record-and-return
// model: the handler returns even with an already-cancelled context, i.e. it
// never selects on ctx.Done(). A blocking clarifying-question tool (waiting for
// the human) is exactly what the stall guard would kill — this test is the
// guard against someone reintroducing that shape.
func TestToolAskUserReturnsWithoutWaitingOnContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already done: a ctx-waiting handler would fail or hang here
	type ret struct {
		out json.RawMessage
		err error
	}
	done := make(chan ret, 1)
	go func() {
		out, err := toolAskUser(ctx, nil, json.RawMessage(`{"question":"Which branch should I target?","options":["develop","main"]}`))
		done <- ret{out, err}
	}()
	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("toolAskUser error: %v", r.err)
		}
		var got struct {
			Recorded bool   `json:"recorded"`
			Question string `json:"question"`
			Options  []struct {
				Label string `json:"label"`
			} `json:"options"`
		}
		if err := json.Unmarshal(r.out, &got); err != nil {
			t.Fatalf("unmarshal result: %v", err)
		}
		if !got.Recorded {
			t.Error("result must report recorded:true")
		}
		if got.Question != "Which branch should I target?" {
			t.Errorf("question = %q, want it echoed intact", got.Question)
		}
		if len(got.Options) != 2 || got.Options[0].Label != "develop" || got.Options[1].Label != "main" {
			t.Errorf("options = %+v, want develop + main intact", got.Options)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("toolAskUser blocked — it must RECORD the question and return immediately")
	}
}

// TestToolAskUserRefusesMalformedLoudly: the handler propagates the parse
// error, which becomes the tool RESULT the model reads.
func TestToolAskUserRefusesMalformedLoudly(t *testing.T) {
	if _, err := toolAskUser(context.Background(), nil, json.RawMessage(`{"question":"Pick","options":["one"]}`)); err == nil {
		t.Fatal("a single-option call must be refused loudly")
	}
}

// TestAskUserRegistryEntry: the tool is a registered, non-mutating product tool.
func TestAskUserRegistryEntry(t *testing.T) {
	reg := NewToolRegistry(nil, slog.Default(), nil)
	td, ok := reg.Get("ask_user")
	if !ok {
		t.Fatal("ask_user must be registered in the product registry (the ONE registration that reaches both transports)")
	}
	if td.Mutating {
		t.Error("ask_user must not be Mutating — it changes no platform data")
	}
	if reg.IsMutating("ask_user") {
		t.Error("IsMutating(ask_user) = true, want false")
	}
	if td.Fn == nil {
		t.Error("ask_user must carry a handler Fn")
	}
	var hasQuestion bool
	for _, r := range td.Required {
		if r == "question" {
			hasQuestion = true
		}
	}
	if !hasQuestion {
		t.Errorf("Required = %v, want it to include question", td.Required)
	}
}

// TestAskUserAllowedInEveryMode: internal/askmode is a DENY list, so ask_user is
// allowed in all three modes without a policy edit.
func TestAskUserAllowedInEveryMode(t *testing.T) {
	for _, mode := range []string{askmode.Brainstorm, askmode.Iteration, askmode.QuickWork} {
		if !askmode.Allows(mode, "ask_user") {
			t.Errorf("askmode.Allows(%q, ask_user) = false — asking must be allowed in every mode", mode)
		}
	}
}

// TestAskUserPromptTeachesTheToolNotProse asserts the prompt-level half of the
// "model calls ask_user rather than a numbered list in prose" AC: the shared
// tool list carries the tool name AND the explicit "do not write a numbered
// list" rule, and Brainstorm's clarifying-questions principle names the tool.
func TestAskUserPromptTeachesTheToolNotProse(t *testing.T) {
	reg := NewToolRegistry(nil, slog.Default(), nil)
	for _, mode := range []string{modeBrainstorm, modeIteration, modeQuickWork} {
		p := BuildSystemPrompt(mode, db.AgentConfigRow{}, reg)
		if !strings.Contains(p, "orchicon_ask_user") {
			t.Errorf("mode %q prompt does not name orchicon_ask_user", mode)
		}
		if !strings.Contains(p, "Do NOT write a numbered list") {
			t.Errorf("mode %q prompt does not carry the 'do not write a numbered list' rule", mode)
		}
	}
	// Brainstorm's clarifying-questions principle must route the question
	// through the tool, not prose.
	bp := BuildSystemPrompt(modeBrainstorm, db.AgentConfigRow{}, reg)
	if !strings.Contains(bp, "Put the question to the user with orchicon_ask_user") {
		t.Error("Brainstorm principle #2 must route clarifying questions through orchicon_ask_user")
	}
}
