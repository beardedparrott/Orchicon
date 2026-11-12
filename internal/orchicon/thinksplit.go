package orchicon

import "strings"

// thinksplit.go — inline-reasoning routing for OpenAI-compatible streams whose server does NOT
// split reasoning into a reasoning field (observed.

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

// feed consumes one content delta and appends the resulting events to the queue: TextDelta for
// plain content, ReasoningDelta for in-tag content.
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

// splitHoldback returns how many TRAILING bytes of s must be held back so that a tag split across
// the chunk boundary is not emitted as plain text.
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

// drain flushes any held-back text at end-of-stream (a truncated final tag or a plain suffix that
// never grew into a tag): held-back text in content mode is content, in think mode it is reasoning.
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
