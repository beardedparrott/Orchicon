package control

// budget_warning_text_test.go — the budget ladder's MESSAGES on the settings form.
//
// "Under settings, I see the budget settings now but none of the budget warning
// text settings are there."
//
// The gates and the ladder THRESHOLDS had been expanded out of the blob; the
// twelve per-tier message strings had not, so an operator could tune when the
// ladder fired but not a word of what the worker reads when it does. The GUI has
// carried these since the ladder shipped (frontend/src/routes/settings.tsx,
// BudgetWarningsEditor), which is what made this a parity gap rather than a new
// feature.

import (
	"encoding/json"
	"testing"
)

// allWarningMessageFields is the coverage contract: 4 dimensions x 3 tiers, the
// exact set the GUI's BudgetWarningsEditor renders.
var allWarningMessageFields = []string{
	"warn_msg_tokens", "esc_msg_tokens", "final_msg_tokens",
	"warn_msg_cost", "esc_msg_cost", "final_msg_cost",
	"warn_msg_tools", "esc_msg_tools", "final_msg_tools",
	"warn_msg_time", "esc_msg_time", "final_msg_time",
}

// The complaint was that the settings were MISSING, so this asserts the form
// RENDERS a field for each one. Testing only budgetInitials/buildBudgetJSON would
// pass just as happily on a form that displays nothing.
func TestSettingsFormCarriesEveryWarningMessageField(t *testing.T) {
	f := (&Model{}).settingsForm()
	have := map[string]bool{}
	for _, s := range f.Specs {
		have[s.Name] = true
	}
	for _, name := range allWarningMessageFields {
		if !have[name] {
			t.Errorf("the settings form has no field %q — the warning text is not editable from the TUI", name)
		}
	}
	// The thresholds must survive alongside them: both halves of the ladder are
	// edited on the same form.
	for _, name := range []string{"warn_frac_tokens", "warn_frac_cost", "warn_frac_tools", "warn_frac_time"} {
		if !have[name] {
			t.Errorf("the ladder threshold field %q went missing", name)
		}
	}
}

// The message fields seed from the transport blob, so the form shows the copy the
// worker would actually receive rather than a blank slate.
func TestWarningMessagesAreSeededFromTheBlob(t *testing.T) {
	blob := `{"tokens":500000,"warnings":{
	  "fractions":{"tokens":[0.25,0.5,0.75]},
	  "messages":{
	    "tokens":["near the limit","closer still","last chance"],
	    "cost_usd":["","",""],
	    "tool_call_count":["t1","t2","t3"]
	  }}}`
	initials := budgetInitials(blob)

	for _, tc := range []struct{ field, want string }{
		{"warn_msg_tokens", "near the limit"},
		{"esc_msg_tokens", "closer still"},
		{"final_msg_tokens", "last chance"},
		{"warn_msg_tools", "t1"},
		{"esc_msg_tools", "t2"},
		{"final_msg_tools", "t3"},
		// A dimension the blob carries as blanks reads as blanks.
		{"warn_msg_cost", ""},
		{"final_msg_cost", ""},
		// A dimension the blob omits entirely must still be seeded (as blank), or
		// the field would be absent from the form's value map on submit.
		{"warn_msg_time", ""},
		{"final_msg_time", ""},
	} {
		if got := initials[tc.field]; got != tc.want {
			t.Errorf("initial %s = %q, want %q", tc.field, got, tc.want)
		}
	}
}

// A save that changes nothing reproduces the messages: the form seeds them from
// the blob and composes them back. A one-way conversion would wipe the operator's
// copy on every unrelated settings save.
func TestWarningMessagesRoundTripUnchanged(t *testing.T) {
	blob := `{"warnings":{"messages":{
	  "tokens":["a","b","c"],
	  "cost_usd":["d","e","f"],
	  "tool_call_count":["g","h","i"],
	  "wall_clock_seconds":["j","k","l"]}}}`
	out := buildBudgetJSON(budgetInitials(blob))

	var got struct {
		Warnings struct {
			Messages map[string][3]string `json:"messages"`
		} `json:"warnings"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("invalid JSON: %v (%s)", err, out)
	}
	for _, tc := range []struct {
		dim  string
		want [3]string
	}{
		{"tokens", [3]string{"a", "b", "c"}},
		{"cost_usd", [3]string{"d", "e", "f"}},
		{"tool_call_count", [3]string{"g", "h", "i"}},
		{"wall_clock_seconds", [3]string{"j", "k", "l"}},
	} {
		if got.Warnings.Messages[tc.dim] != tc.want {
			t.Errorf("%s messages = %v, want %v", tc.dim, got.Warnings.Messages[tc.dim], tc.want)
		}
	}
}

// Fractions and messages are two keys of ONE warnings object. Writing a message
// must not drop the thresholds that were set on the same form.
func TestMessagesAndFractionsShareTheWarningsBlock(t *testing.T) {
	out := buildBudgetJSON(map[string]string{
		"warn_frac_tokens": "0.25,0.5,0.75",
		"warn_msg_tokens":  "slow down",
	})
	var got struct {
		Warnings map[string]any `json:"warnings"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("invalid JSON: %v (%s)", err, out)
	}
	if _, ok := got.Warnings["fractions"]; !ok {
		t.Errorf("setting a message dropped the fractions: %s", out)
	}
	if _, ok := got.Warnings["messages"]; !ok {
		t.Errorf("setting a threshold dropped the messages: %s", out)
	}
}

// An untouched message block writes NO warnings key at all, so an unrelated
// settings save cannot overwrite stored copy with blanks. (An absent key means
// "keep" to the server; the columns are NOT NULL DEFAULT, so they are never
// legitimately empty.)
func TestUntouchedMessagesWriteNoWarningsBlock(t *testing.T) {
	out := buildBudgetJSON(map[string]string{})
	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if _, present := got["warnings"]; present {
		t.Errorf("a form with no ladder values wrote a warnings block: %v", got["warnings"])
	}
}

// A dimension with only ONE tier filled writes the whole triple — the server reads
// `messages.<dim>` as a unit. The unfilled tiers go as "", which SILENCES them
// (the adapter assigns each string verbatim), so this is a real edit and the
// behaviour is deliberate rather than a fallback.
func TestOneFilledTierWritesTheWholeTriple(t *testing.T) {
	out := buildBudgetJSON(map[string]string{"esc_msg_tokens": "tighter now"})
	var got struct {
		Warnings struct {
			Messages map[string][3]string `json:"messages"`
		} `json:"warnings"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("invalid JSON: %v (%s)", err, out)
	}
	if len(got.Warnings.Messages) != 1 {
		t.Fatalf("messages = %v, want only the tokens dimension", got.Warnings.Messages)
	}
	if got.Warnings.Messages["tokens"] != [3]string{"", "tighter now", ""} {
		t.Errorf("tokens triple = %v, want the blanks preserved alongside the text",
			got.Warnings.Messages["tokens"])
	}
}

// Surrounding whitespace is trimmed so a stray newline from a textarea paste does
// not become part of the injected message.
func TestMessageWhitespaceIsTrimmed(t *testing.T) {
	out := buildBudgetJSON(map[string]string{"warn_msg_tokens": "  padded\n"})
	var got struct {
		Warnings struct {
			Messages map[string][3]string `json:"messages"`
		} `json:"warnings"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if got.Warnings.Messages["tokens"][0] != "padded" {
		t.Errorf("warn message = %q, want %q", got.Warnings.Messages["tokens"][0], "padded")
	}
}

// A whitespace-only set of fields counts as untouched: three spaces is not a
// message, and treating it as one would overwrite the stored copy with blanks.
func TestWhitespaceOnlyMessagesCountAsUntouched(t *testing.T) {
	out := buildBudgetJSON(map[string]string{
		"warn_msg_tokens": "  ", "esc_msg_tokens": "\t", "final_msg_tokens": "\n",
	})
	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if _, present := got["warnings"]; present {
		t.Errorf("whitespace-only messages wrote a warnings block: %v", got["warnings"])
	}
}

// A message containing a comma or a quote must survive JSON encoding intact —
// the real copy is prose and contains both.
func TestMessageCopyWithPunctuationSurvivesEncoding(t *testing.T) {
	copy := `Stop, look, and listen — you're at {pct}% and "batch" is the answer.`
	out := buildBudgetJSON(map[string]string{"warn_msg_tokens": copy})
	var got struct {
		Warnings struct {
			Messages map[string][3]string `json:"messages"`
		} `json:"warnings"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("invalid JSON: %v (%s)", err, out)
	}
	if got.Warnings.Messages["tokens"][0] != copy {
		t.Errorf("copy mangled by encoding:\n got %q\nwant %q", got.Warnings.Messages["tokens"][0], copy)
	}
}
