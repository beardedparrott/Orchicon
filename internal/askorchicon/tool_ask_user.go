package askorchicon

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/beardedparrott/orchicon/internal/db"
)

// ask_user — the interactive clarifying-question tool the clients render as a card.
//
// RECORD-AND-RETURN, NOT BLOCKING. This handler RECORDS the question (it is the
// tool CALL, persisted to the durable transcript with its question and options
// intact) and RETURNS IMMEDIATELY. The turn then ends normally; the client
// renders the recorded call as a card, and selecting an option sends it as the
// NEXT user message. There is no rendezvous, no new streaming surface, and it
// works identically on both Ask transports because it is just transcript
// content — so it composes with compaction, resume and the follow-up path for
// free.
//
// THE SHAPE IS FORCED BY THE STALL GUARD. chatStallMonitor.toolWedge()
// (stall.go) reclaims any tool left open with no events for the wedge window; a
// human taking three minutes to read options would trip exactly the mechanism
// built to catch a wedged tool. A BLOCKING ask_user (waiting for the human on
// ctx.Done()) is therefore impossible here — it would be cancelled mid-question
// and the turn failed. This handler never waits on ctx.
//
// THE DISTINCTION THIS MUST NOT LOSE. The CONSENT ask (an approval flowing back
// over the opencode permission channel) is genuinely BLOCKING on the transport
// side: the turn waits for a reply. This CLARIFYING-QUESTION ask is NOT. The two
// share a rendering primitive (the client card) and a reply-as-message pattern,
// and they must NOT share a blocking model. If a future consent tool is ever
// routed through this registry it must not copy this handler's record-and-return
// shape into a blocking decision, or vice versa.
//
// It is registered as a PRODUCT tool in allTools (tools.go), so ONE registration
// reaches both transports: the native path enumerates it via
// nativeAskTools.AskToolDefs (toolRegistry.List()) and the opencode path via the
// `orchicon mcp` sidecar (mcp.NewAskOrchiconRegistry). No transport-specific code.
//
// Why the handler persists nothing itself: durability is already the existing
// live tool ledger (tool_ledger.go) mirrored into ask_orchicon_messages
// .tool_calls/.tool_results, and db.ListMessages has no compaction filter — so a
// recorded call survives compaction and resume for free. Writing from the
// opencode sidecar (`orchicon mcp` is a SEPARATE process holding the pool) would
// be a second writer racing the collector's own row, which is why the handler
// records nothing.

// askUserOption is one choice-shaped option. It accepts BOTH shapes a model
// emits — a plain JSON string label, or an object {"label":…,"description":…} —
// because the flat PropertySchema cannot express the item shape and models are
// loose about it. The lenient UnmarshalJSON normalises to {label, description?}.
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

// parseAskUserArgs validates and normalises the tool arguments. PURE (no DB, no
// ctx) so it is unit-testable directly.
//
// It refuses LOUDLY, with an error that names the fix: a non-empty question,
// every option carrying a non-empty label, and the "at least two options, or an
// explicit free-text-only form (no options + allow_other=true)" rule. The error
// becomes the tool RESULT the model reads (chatturn.go), so it must be
// actionable, not merely a rejection.
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
	// The rule: two or more choice-shaped options, OR the explicit
	// free-text-only form (no options + allow_other=true). Anything else —
	// including a single option — is refused with the fix named.
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

// toolAskUser records the clarifying question and returns immediately. It never
// waits on ctx (see the file header): the recorded call IS the durable artifact,
// and the user answers in their next message.
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
