package askorchicon

import (
	"strings"

	"github.com/beardedparrott/orchicon/internal/opencode"
)

// Folded-think boundary markers are the SHARED opencode tag table (opencode.ThinkOpenTags /
// ThinkCloseTags): pipe-delimited GLM markers ("|<thinking>".

// thinkSegState is the segmenter's carry-over state across deltas.
type thinkSegState int

const (
	// thinkSegText is outside a think block — emitting plain text.
	thinkSegText thinkSegState = iota
	// thinkSegInOpenTag is still recognizing an open tag (partial chunk).
	thinkSegInOpenTag
	// thinkSegInBody is inside a think block — accumulating body.
	thinkSegInBody
	// thinkSegInCloseTag is still recognizing a close tag (partial chunk).
	thinkSegInCloseTag
)

// thinkSegmenter scans a TEXT delta stream and splits it into plain text and folded think-body
// fragments.
type thinkSegmenter struct {
	state thinkSegState
	// buf accumulates a partial think body (in-body accumulator).
	buf strings.Builder
	// pendingOpen holds a partial open-tag prefix awaiting more deltas.
	pendingOpen strings.Builder
	// pendingClose holds a partial close-tag prefix awaiting more deltas.
	pendingClose strings.Builder
	// bodyStarted reports that an open tag was fully recognized and we are
	// accumulating a think body.
	bodyStarted bool
}

// reset returns the segmenter to its initial state (plain-text scanning).
func (seg *thinkSegmenter) reset() {
	seg.state = thinkSegText
	seg.buf.Reset()
	seg.pendingOpen.Reset()
	seg.pendingClose.Reset()
	seg.bodyStarted = false
}

// feed consumes one text delta, emitting plain text via emitText, think-body content via growThink
// (incremental), and the completed body via commitThink.
func (seg *thinkSegmenter) feed(delta string, emitText func(string), growThink func(string), commitThink func(string)) {
	for i := 0; i < len(delta); {
		switch seg.state {
		case thinkSegText:
			// Emit a maximal run of plain (non-tag-start) bytes in one
			// callback rather than one byte per call.
			j := i
			for j < len(delta) && delta[j] != '|' && delta[j] != '<' {
				j++
			}
			if j > i {
				if txt := delta[i:j]; txt != "" && emitText != nil {
					emitText(txt)
				}
				i = j
				continue
			}
			// delta[i] starts a potential open tag — begin buffering it.
			seg.state = thinkSegInOpenTag
			seg.pendingOpen.WriteByte(delta[i])
			i++
		case thinkSegInOpenTag:
			seg.pendingOpen.WriteByte(delta[i])
			p := seg.pendingOpen.String()
			if seg.isCompleteOpen(p) {
				seg.state = thinkSegInBody
				seg.pendingOpen.Reset()
				seg.bodyStarted = true
			} else if !seg.couldBeOpenPrefix(p) {
				// Not a tag after all — flush the accumulated text.
				if emitText != nil {
					emitText(p)
				}
				seg.pendingOpen.Reset()
				seg.state = thinkSegText
			}
			i++
		case thinkSegInBody:
			if delta[i] == '|' || delta[i] == '<' {
				seg.state = thinkSegInCloseTag
				seg.pendingClose.WriteByte(delta[i])
			} else {
				// Emit the body incrementally so the mirror's reasoning tail
				// grows live, and accumulate it for the commit boundary.
				seg.buf.WriteByte(delta[i])
				if growThink != nil {
					growThink(string(delta[i]))
				}
			}
			i++
		case thinkSegInCloseTag:
			seg.pendingClose.WriteByte(delta[i])
			p := seg.pendingClose.String()
			if seg.isCompleteClose(p) {
				seg.commit(commitThink)
				seg.state = thinkSegText
			} else if !seg.couldBeClosePrefix(p) {
				// Not a close tag — the body text continues.
				seg.buf.WriteString(p)
				if growThink != nil {
					growThink(p)
				}
				seg.pendingClose.Reset()
				seg.state = thinkSegInBody
			}
			i++
		}
	}
}

// commit emits the accumulated body via commitThink (if any) and resets the
// in-body accumulator so the next think block starts clean.
func (seg *thinkSegmenter) commit(commitThink func(string)) {
	if b := seg.buf.String(); b != "" && commitThink != nil {
		commitThink(b)
	}
	seg.buf.Reset()
	seg.pendingClose.Reset()
	seg.bodyStarted = false
}

// flushBody closes an unterminated think block (provider truncation / abort / supersede at turn
// end) by committing any buffered body.
func (seg *thinkSegmenter) flushBody(commitThink func(string)) {
	if seg.bodyStarted {
		seg.commit(commitThink)
	}
	seg.reset()
}

// inThink reports whether the segmenter currently has a think run open (a fully recognized open tag
// with no close yet).
func (seg *thinkSegmenter) inThink() bool {
	return seg.state == thinkSegInBody || seg.state == thinkSegInCloseTag || seg.bodyStarted
}

// --- prefix helpers (against the shared opencode tag table) ---

// isCompleteOpen reports whether the buffered string is a complete open tag.
func (seg *thinkSegmenter) isCompleteOpen(s string) bool {
	for _, m := range opencode.ThinkOpenTags {
		if s == m {
			return true
		}
	}
	return false
}

// couldBeOpenPrefix reports whether the buffered string is still a prefix of
// a recognized open marker (i.e. it may yet grow into a complete open tag).
func (seg *thinkSegmenter) couldBeOpenPrefix(s string) bool {
	for _, m := range opencode.ThinkOpenTags {
		if strings.HasPrefix(m, s) {
			return true
		}
	}
	return false
}

// isCompleteClose reports whether the buffered string is a complete close tag.
func (seg *thinkSegmenter) isCompleteClose(s string) bool {
	for _, m := range opencode.ThinkCloseTags {
		if s == m {
			return true
		}
	}
	return false
}

// couldBeClosePrefix reports whether the buffered string is still a prefix of
// a recognized close marker.
func (seg *thinkSegmenter) couldBeClosePrefix(s string) bool {
	for _, m := range opencode.ThinkCloseTags {
		if strings.HasPrefix(m, s) {
			return true
		}
	}
	return false
}
