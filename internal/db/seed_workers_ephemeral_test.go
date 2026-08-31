package db

import (
	"strings"
	"testing"
)

// TestResearchTrioEphemeralSeed is acceptance criterion #6: the
// Automation Research trio's seed definitions must NOT carry the
// unconditional "commit + push to the run branch" contract — the run is
// ephemeral (`git_strategy=none`), so the seed must tell the worker the
// worktree is a detached HEAD with nothing pushed. The trio's RollMarker
// must be the ephemeral fragment so ONLY the trio re-rolls (never the whole
// fleet), and the old push contract must be gone from the seeded AGENTS.md.
func TestResearchTrioEphemeralSeed(t *testing.T) {
	slugs := map[string]bool{
		"automation-research-planner":     false,
		"automation-research-analyst":     false,
		"automation-research-synthesizer": false,
	}
	for _, w := range cannedWorkers {
		if _, ok := slugs[w.Slug]; !ok {
			continue
		}
		slugs[w.Slug] = true
		if w.RollMarker != researchEphemeralMarker {
			t.Errorf("%s RollMarker = %q, want researchEphemeralMarker %q", w.Slug, w.RollMarker, researchEphemeralMarker)
		}
		if !strings.Contains(w.AgentsMD, researchEphemeralMarker) {
			t.Errorf("%s seed AgentsMD lacks the ephemeral marker fragment %q", w.Slug, researchEphemeralMarker)
		}
		if strings.Contains(w.AgentsMD, "push to the run branch") {
			t.Errorf("%s seed still instructs push to the run branch (ephemeral run must not)", w.Slug)
		}
		if strings.Contains(w.Behavior, "push to the run branch") {
			t.Errorf("%s Behavior still instructs push to the run branch", w.Slug)
		}
	}
	for slug, seen := range slugs {
		if !seen {
			t.Errorf("research trio member %q not found in cannedWorkers", slug)
		}
	}
}
