package opencode

import "testing"

// TestThinkTagTableHoldsPlainThinkPair is the acceptance-A drift guard:
// the ONE shared folded-think tag table both consumers match against must
// contain the plain "<think>" / "</think>" pair. Literals are built by
// concatenation — raw markup is never typed verbatim in this repo because
// tooling rewrites it in transit (see think_tags.go).
//
// internal/askorchicon/thinksegmenter.go (Ask live deltas + completed
// parts) and think_demux.go (execution transcripts) both range over THESE
// slices, so contents pinned here can never drift between the two paths.
func TestThinkTagTableHoldsPlainThinkPair(t *testing.T) {
	open := "<" + "think>"
	close := "<" + "/think>"
	if !containsThinkTag(ThinkOpenTags, open) {
		t.Errorf("ThinkOpenTags %q missing plain open tag", ThinkOpenTags)
	}
	if !containsThinkTag(ThinkCloseTags, close) {
		t.Errorf("ThinkCloseTags %q missing plain close tag", ThinkCloseTags)
	}
	// The pipe-delimited GLM spellings that PR 485 handled must survive too.
	pipeOpen := "|" + "<thinking>"
	if !containsThinkTag(ThinkOpenTags, pipeOpen) {
		t.Errorf("ThinkOpenTags %q missing pipe open tag", ThinkOpenTags)
	}
}

func containsThinkTag(tags []string, want string) bool {
	for _, g := range tags {
		if g == want {
			return true
		}
	}
	return false
}
