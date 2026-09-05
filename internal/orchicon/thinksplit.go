package orchicon

import "strings"

// thinksplit.go — inline-reasoning routing for OpenAI-compatible streams
// whose server does NOT split reasoning into a reasoning field (observed:
// llama.cpp's llama-server with the default reasoning format emits the
// model's chain of thought INLINE in delta.content, wrapped in the
// "think" tag pair) — the tags then rendered raw in the execution chat
// while Ask Orchicon's chat already surfaced its thinking as bubbles.
//
// The splitter is a streaming state machine over content deltas: text
// outside the tag pair is passed through as content, text inside is
// routed as reasoning. It tolerates TAGS SPLIT ACROSS CHUNK BOUNDARIES
// (an SSE frame boundary can land mid-tag — the classic "<th" | "ink>"
// split) by holding back a suffix that could still be a tag prefix; the
// holdback is re-emitted on the next delta, so worst case a chunk's text
// arrives a few bytes later.
//
// Scope guard: only the openai-compat wire feeds this (llama.cpp /
// llama-serve); the opencode route already has native reasoning fields
// and the anthropic wire has thinking blocks — both stay untouched.

// The tag literals are built by concatenation (never one verbatim literal
// in source) so tooling that rewrites raw markup cannot corrupt them.
var (
	thinkOpenTag  = "<" + "think" + ">"
	thinkCloseTag = "</" + "think" + ">"
)

// thinkSplitter carries the cross-chunk state.
type thinkSplitter struct {
	inThink bool
	// holdback is text that MIGHT be a tag prefix (or its closing tag)
	// split across chunks. Never emitted until proven plain text.
	holdback strings.Builder
}

func newThinkSplitter() *thinkSplitter {
	return &thinkSplitter{}
}

// feed consumes one content delta and appends the resulting events to the
// queue: TextDelta for plain content, ReasoningDelta for in-tag content.
// Exactly one of the two is produced per input byte; the tag delimiters
// themselves are never emitted.
func (t *thinkSplitter) feed(content string, queue *[]Event) {
	if content == "" {
		return
	}
	t.holdback.WriteString(content)
	s := t.holdback.String()
	t.holdback.Reset()

	for s != "" {
		if t.inThink {
			// Close the tag: find the nearest close delimiter. Anything
			// before it is reasoning.
			idx := strings.Index(s, thinkCloseTag)
			if idx >= 0 {
				if idx > 0 {
					*queue = append(*queue, ReasoningDelta{Text: s[:idx]})
				}
				t.inThink = false
				s = s[idx+len(thinkCloseTag):]
				continue
			}
			// Hold back a suffix that could still be a split close tag;
			// emit the rest as reasoning.
			keep := splitHoldback(s, thinkCloseTag)
			if emit := s[:len(s)-keep]; emit != "" {
				*queue = append(*queue, ReasoningDelta{Text: emit})
			}
			if keep > 0 {
				t.holdback.WriteString(s[len(s)-keep:])
			}
			return
		}
		// Open the tag.
		idx := strings.Index(s, thinkOpenTag)
		if idx >= 0 {
			if idx > 0 {
				*queue = append(*queue, TextDelta{Text: s[:idx]})
			}
			t.inThink = true
			s = s[idx+len(thinkOpenTag):]
			continue
		}
		// Hold back a suffix that could be a split open tag; emit the rest.
		keep := splitHoldback(s, thinkOpenTag)
		if emit := s[:len(s)-keep]; emit != "" {
			*queue = append(*queue, TextDelta{Text: emit})
		}
		if keep > 0 {
			t.holdback.WriteString(s[len(s)-keep:])
		}
		return
	}
}

// splitHoldback returns how many TRAILING bytes of s must be held back so
// that a tag split across the chunk boundary is not emitted as plain
// text: the longest suffix of s that is a proper prefix of the tag.
func splitHoldback(s, tag string) int {
	max := len(tag) - 1
	if len(s) < max {
		max = len(s)
	}
	for k := max; k > 0; k-- {
		if strings.HasPrefix(tag, s[len(s)-k:]) {
			return k
		}
	}
	return 0
}

// drain flushes any held-back text at end-of-stream (a truncated final
// tag or a plain suffix that never grew into a tag): held-back text in
// content mode is content, in think mode it is reasoning.
func (t *thinkSplitter) drain(queue *[]Event) {
	if t.holdback.Len() == 0 {
		return
	}
	s := t.holdback.String()
	t.holdback.Reset()
	if t.inThink {
		*queue = append(*queue, ReasoningDelta{Text: s})
		return
	}
	*queue = append(*queue, TextDelta{Text: s})
}
