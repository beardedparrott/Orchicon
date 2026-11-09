package askorchicon

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/beardedparrott/orchicon/internal/db"
)

// ask_user — the interactive clarifying-question tool the clients render as a card. THE DISTINCTION
// THIS MUST NOT LOSE.

// askUserOption is one choice-shaped option.
type askUserOption struct {
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
}

// UnmarshalJSON accepts a JSON string ("Option A") or a JSON object
// ({"label":"Option A","description":"…"}). Anything else is a loud error.
func (o *askUserOption) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err == nil {
		o.Label = s
		o.Description = ""
		return nil
	}
	// Reject a bare null/empty as an empty-label option (the caller's
	// validation names the fix) rather than a decode failure.
	type plain askUserOption
	var p plain
	if err := json.Unmarshal(data, &p); err != nil {
		return fmt.Errorf("option must be a string label or an object {label, description?}: %w", err)
	}
	*o = askUserOption(p)
	return nil
}

// parseAskUserArgs validates and normalises the tool arguments.
func parseAskUserArgs(raw []byte) (question string, options []askUserOption, allowOther bool, err error) {
	if len(strings.TrimSpace(string(raw))) == 0 {
		return "", nil, false, fmt.Errorf("ask_user: missing arguments — call it with at least " +
			"{\"question\":\"…\",\"options\":[{\"label\":\"…\"},{\"label\":\"…\"}]}.")
	}
	var in struct {
		Question   string          `json:"question"`
		Options    []askUserOption `json:"options"`
		AllowOther bool            `json:"allow_other"`
	}
	if uerr := json.Unmarshal(raw, &in); uerr != nil {
		return "", nil, false, fmt.Errorf("ask_user: arguments are not a valid JSON object (%v) — expected "+
			"{\"question\":\"…\",\"options\":[…]} with options as an array.", uerr)
	}
	question = strings.TrimSpace(in.Question)
	if question == "" {
		return "", nil, false, fmt.Errorf("ask_user: `question` is required and must not be empty — ask one clear " +
			"question in one or two sentences.")
	}
	opts := make([]askUserOption, 0, len(in.Options))
	for i, o := range in.Options {
		label := strings.TrimSpace(o.Label)
		if label == "" {
			return "", nil, false, fmt.Errorf("ask_user: option %d has an empty label — every option needs a "+
				"non-empty label.", i+1)
		}
		opts = append(opts, askUserOption{Label: label, Description: strings.TrimSpace(o.Description)})
	}
	// The rule: two or more choice-shaped options, OR the explicit free-text-only form (no options +
	// allow_other=true).
	if len(opts) == 0 && !in.AllowOther {
		return "", nil, false, fmt.Errorf("ask_user: provide at least two options, or set allow_other=true for a " +
			"free-text-only question.")
	}
	if len(opts) == 1 {
		return "", nil, false, fmt.Errorf("ask_user: provide at least two options, or set allow_other=true for a "+
			"free-text-only question (got 1 option: %q) — a single choice is not a choice.", opts[0].Label)
	}
	return question, opts, in.AllowOther, nil
}

// toolAskUser records the clarifying question and returns immediately.
func toolAskUser(_ context.Context, _ *db.Pool, args json.RawMessage) (json.RawMessage, error) {
	question, options, allowOther, err := parseAskUserArgs(args)
	if err != nil {
		return nil, err
	}
	if options == nil {
		options = []askUserOption{}
	}
	out := struct {
		Recorded   bool            `json:"recorded"`
		Question   string          `json:"question"`
		Options    []askUserOption `json:"options"`
		AllowOther bool            `json:"allow_other"`
		Note       string          `json:"note"`
	}{
		Recorded:   true,
		Question:   question,
		Options:    options,
		AllowOther: allowOther,
		Note: "The question is recorded and shown to the user as a card. END YOUR TURN NOW — the user answers " +
			"in their next message. Do not guess the answer and do not continue working.",
	}
	return json.Marshal(out)
}
