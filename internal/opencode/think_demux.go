package opencode

import "strings"

// think_demux.go — folded-think segmentation for COMPLETED text parts on the opencode transport
// (worker execution transcripts).

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

// segment splits one completed text part: out-of-think runs are joined into clean (possibly empty
// when the part carried no assistant text).
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
