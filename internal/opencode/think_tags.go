package opencode

// think_tags.go — the ONE shared folded-think tag table for the opencode
// transport (worker execution transcripts) and Ask Orchicon (live delta
// stream + completed parts).
//
// GLM-style models interleave thinking segments INLINE in the content
// stream wrapped in tag pairs; different models use different spellings
// (pipe-delimited "|<thinking>" vs plain "<think>" vs "<thought>" vs
// "<reasoning>"). Both consumers match against THESE slices, so a newly
// observed spelling is added once here and can never drift between the two
// paths again. Semantics are strip-and-route-to-reasoning only.
//
// The literals are built by concatenation (never one verbatim literal in
// source) so tooling that rewrites raw markup cannot corrupt them — the
// same convention think_demux.go and the Ask segmenter already used.
var (
	thinkOpenPipeThinking = "|" + "<thinking>"
	thinkOpenThinking     = "<" + "thinking>"
	thinkOpenPipeThink    = "|" + "think"
	thinkOpenThink        = "<" + "think>"
	thinkOpenThought      = "<" + "thought>"
	thinkOpenReasoning    = "<" + "reasoning>"

	thinkClosePipeThinking = "|" + "</thinking>"
	thinkCloseThinking     = "<" + "/thinking>"
	thinkClosePipeThink    = "|" + "/think"
	thinkCloseThink        = "<" + "/think>"
	thinkCloseThought      = "<" + "/thought>"
	thinkCloseReasoning    = "<" + "/reasoning>"
)

// ThinkOpenTags is the shared set of folded-think open-tag literals.
// Case-sensitive match only; case variants are deferred unless a repro
// shows them in the wild.
var ThinkOpenTags = []string{
	thinkOpenPipeThinking,
	thinkOpenThinking,
	thinkOpenPipeThink,
	thinkOpenThink,
	thinkOpenThought,
	thinkOpenReasoning,
}

// ThinkCloseTags is the shared set of folded-think close-tag literals.
var ThinkCloseTags = []string{
	thinkClosePipeThinking,
	thinkCloseThinking,
	thinkClosePipeThink,
	thinkCloseThink,
	thinkCloseThought,
	thinkCloseReasoning,
}
