package askorchicon

import "strings"

// Folded-think boundary markers used by the opencode/GLM transport to embed
// additional thinking segments inline in the content stream. The segmenter
// recognizes these with carry-over state so a tag split across multiple token
// deltas is still demuxed. Exact literals are exported constants so a
// provider-profile correction only needs one line.
const (
	// thinkOpenLiteral is the primary pipe-delimited open tag.
	thinkOpenLiteral = "|<thinking>"
	// thinkOpenAlt is the bare-form open tag.
	thinkOpenAlt = "<thinking>"
	// thinkOpenPipe is the pipe-form open tag.
	thinkOpenPipe = "|think"
	// thinkCloseLiteral is the primary pipe-delimited close tag.
	thinkCloseLiteral = "|</thinking>"
	// thinkCloseAlt is the bare-form close tag.
	thinkCloseAlt = "</thinking>"
	// thinkClosePipe is the pipe-form close tag.
	thinkClosePipe = "|/think"
)

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

// thinkSegmenter scans a TEXT delta stream and splits it into plain text and
// folded think-body fragments. Carry-over state makes it robust to tags split
// across any number of deltas. It is REPLACED (not reused) at the completed-
// text-part boundary, so state never leaks between a streamed text part and
// the completed part that subsumes it.
//
// The state machine deliberately avoids whole-string equality on the delta
// stream: an open tag that arrives split across three deltas (e.g. "|<t" then
// "hink" then "ing>") is recognized enter/exit one rune at a time, buffering a
// candidate prefix and only flushing it as plain text once it provably stops
// being a prefix of any recognized marker.
//
// feed emits through three callbacks so the caller can both GROW the live
// reasoning tail incrementally (so a re-attached client sees the thinking
// bubble grow) and COMMIT a terminated body once:
//
//   - emitText(chunk): plain (non-folded) text the caller renders/mirrors.
//   - growThink(chunk): think-body content as it streams (mirror tail growth).
//   - commitThink(body): the FULL body, called exactly once per think block —
//     at close-tag termination, or by flushBody for an unterminated block.
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

// feed consumes one text delta, emitting plain text via emitText, think-body
// content via growThink (incremental), and the completed body via commitThink.
// Any of the three callbacks may be nil. State carries ACROSS calls so a tag
// split over multiple deltas is recognized.
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

// flushBody closes an unterminated think block (provider truncation / abort /
// supersede at turn end) by committing any buffered body. The state resets to
// plain text so subsequent deltas (in the same turn) are treated as text. Only
// confirmed think-body content is emitted — tag fragments never reach the text
// channel.
func (seg *thinkSegmenter) flushBody(commitThink func(string)) {
	if seg.bodyStarted {
		seg.commit(commitThink)
	}
	seg.reset()
}

// --- prefix helpers ----------------------------------------------

// isCompleteOpen reports whether the buffered string is a complete open tag.
func (seg *thinkSegmenter) isCompleteOpen(s string) bool {
	return s == thinkOpenLiteral || s == thinkOpenAlt || s == thinkOpenPipe
}

// couldBeOpenPrefix reports whether the buffered string is still a prefix of
// a recognized open marker (i.e. it may yet grow into a complete open tag).
func (seg *thinkSegmenter) couldBeOpenPrefix(s string) bool {
	for _, m := range []string{thinkOpenLiteral, thinkOpenAlt, thinkOpenPipe} {
		if strings.HasPrefix(m, s) {
			return true
		}
	}
	return false
}

// isCompleteClose reports whether the buffered string is a complete close tag.
func (seg *thinkSegmenter) isCompleteClose(s string) bool {
	return s == thinkCloseLiteral || s == thinkCloseAlt || s == thinkClosePipe
}

// couldBeClosePrefix reports whether the buffered string is still a prefix of
// a recognized close marker.
func (seg *thinkSegmenter) couldBeClosePrefix(s string) bool {
	for _, m := range []string{thinkCloseLiteral, thinkCloseAlt, thinkClosePipe} {
		if strings.HasPrefix(m, s) {
			return true
		}
	}
	return false
}
