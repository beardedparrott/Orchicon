package orchicon

import "testing"

// The pressure gate must measure the FULL input side, not the fresh bucket.
func TestPromptOccupancyIsTheFullInputSide(t *testing.T) {
	u := Usage{
		InputTokens:      168,
		CacheReadTokens:  976384,
		CacheWriteTokens: 0,
		OutputTokens:     1145, // output is NOT part of the prompt
	}
	got := promptOccupancy(u)
	if got != 976552 {
		t.Fatalf("promptOccupancy = %d, want 976552 (fresh + cache read + cache write)", got)
	}
	if got == u.InputTokens {
		t.Fatal("promptOccupancy collapsed to InputTokens — the gate would be blind to cached context again")
	}
	const window = 1048576
	// The measured truth for this session: 93.13% — genuinely close, and genuinely BELOW the 0.95
	// threshold, so the gate is right not to have fired yet.
	frac := float64(got) / float64(window)
	if frac < 0.93 || frac > 0.94 {
		t.Errorf("occupancy/window = %.4f, want ~0.9313 (the measured session)", frac)
	}
	freshFrac := float64(u.InputTokens) / float64(window)
	if freshFrac > 0.001 {
		t.Errorf("fresh bucket alone = %.6f of the window, want ~0.00016 — the fixture must have the bulk cached", freshFrac)
	}
}

// A conversation whose FULL input side clears the threshold must fire, and the same conversation
// measured by its fresh bucket alone must not.
func TestPressureGateFiresOnTheFullInputSideOnly(t *testing.T) {
	const window = 1048576
	const threshold = 0.95
	u := Usage{InputTokens: 200, CacheReadTokens: 1010000} // ~96.4% of the window

	full := float64(promptOccupancy(u)) / float64(window)
	if full < threshold {
		t.Fatalf("full input side = %.4f, want >= %.2f so the gate fires", full, threshold)
	}
	fresh := float64(u.InputTokens) / float64(window)
	if fresh >= threshold {
		t.Fatalf("fresh bucket = %.4f, want < %.2f (it must NOT fire on the fresh bucket)", fresh, threshold)
	}
}

// Cache writes count too: on the turn that stores them they are part of the prompt.
func TestPromptOccupancyCountsCacheWrites(t *testing.T) {
	u := Usage{InputTokens: 100, CacheReadTokens: 200, CacheWriteTokens: 300}
	if got := promptOccupancy(u); got != 600 {
		t.Fatalf("promptOccupancy = %d, want 600", got)
	}
}
