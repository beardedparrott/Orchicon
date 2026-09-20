package opencode

import "strings"

// think_demux.go — folded-think segmentation for COMPLETED text parts on the
// opencode transport (worker execution transcripts).
//
// GLM-style models interleave thinking segments INLINE in the content stream
// wrapped in the "think" tag pair. The Ask collector demuxes the live delta
// stream; here the deltas are deliberately dropped (see TokenDeltaFromBus —
// completed parts carry the durable record), so the leak surfaces one level
// up: a completed `text` part whose body still carries a folded think block
// is persisted verbatim into execution_session_parts and rendered raw in the
// execution chat (found in the wild: a raw think-tag remnant in a persisted
// part). segment() strips those blocks out of completed text BEFORE the part
// reaches parseEvent (output/UI) and recordPart (durable transcript) and
// returns each block body so the caller can persist it as a separate
// reasoning part — the same channel native reasoning parts use. No dedupe:
// a folded block and a native reasoning part are distinct segments.
//
// The splitter is stateful across completed parts within a run (inThink
// persists): a block opened in one part and closed in a later part still
// routes correctly, and each part's in-think runs are emitted eagerly so no
// end-of-run flush — and no cross-goroutine coordination with finish() — is
// ever needed. An unterminated block simply contributes its per-part runs
// as reasoning entries; nothing ever falls back to text.
//
// Scope guard: completed parts only. Deltas stay dropped, native reasoning
// parts stay untouched.

// indexAnyTag returns the earliest occurrence of any tag literal in s and
// the matched literal's length, or idx=-1 when none occurs.
func indexAnyTag(s string, tags []string) (idx, ln int) {
	idx = -1
	for _, t := range tags {
		if i := strings.Index(s, t); i >= 0 && (idx < 0 || i < idx) {
			idx, ln = i, len(t)
		}
	}
	return idx, ln
}

// completedThinkDemux carries the cross-part think state for one session run.
type completedThinkDemux struct {
	inThink bool
}

func newCompletedThinkDemux() *completedThinkDemux {
	return &completedThinkDemux{}
}

// segment splits one completed text part: out-of-think runs are joined into
// clean (possibly empty when the part carried no assistant text), and each
// contiguous in-think run becomes one entry of bodies. Tag delimiters are
// never emitted to either side.
func (d *completedThinkDemux) segment(text string) (clean string, bodies []string) {
	var out strings.Builder
	rest := text
	for rest != "" {
		if d.inThink {
			if idx, ln := indexAnyTag(rest, ThinkCloseTags); idx >= 0 {
				if body := rest[:idx]; body != "" {
					bodies = append(bodies, body)
				}
				d.inThink = false
				rest = rest[idx+ln:]
				continue
			}
			// No close in this part: the whole remainder is thinking.
			// Emit eagerly (no end-of-run flush needed) and stay inThink
			// so a close in a later part still routes correctly.
			bodies = append(bodies, rest)
			rest = ""
			return out.String(), bodies
		}
		if idx, ln := indexAnyTag(rest, ThinkOpenTags); idx >= 0 {
			out.WriteString(rest[:idx])
			d.inThink = true
			rest = rest[idx+ln:]
			continue
		}
		out.WriteString(rest)
		rest = ""
	}
	return out.String(), bodies
}
