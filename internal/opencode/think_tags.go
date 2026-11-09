package opencode

// think_tags.go — the ONE shared folded-think tag table for the opencode transport (worker
// execution transcripts) and Ask Orchicon (live delta stream + completed parts).
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
