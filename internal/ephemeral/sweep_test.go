package ephemeral

import (
	"testing"
	"time"
)

// The TTL resolver is the sweep's only safety instrument, so its failure mode is
// the one worth pinning: a malformed or non-positive env value must NOT collapse
// the window, because a zero window means "everything is abandoned" and the
// sweep would delete live jobs' records.
func TestTTLFromEnvNeverCollapsesTheWindow(t *testing.T) {
	cases := []struct {
		env  string
		want string // the duration we expect, or "" for the default
		why  string
	}{
		{"", "", "unset — the default applies"},
		{"30m", "30m0s", "a Go duration"},
		{"90s", "1m30s", "another duration form"},
		{"3600", "1h0m0s", "a bare number of seconds"},
		{"0", "", "ZERO IS REFUSED — a zero window would make every record look abandoned"},
		{"-5m", "", "negative is refused for the same reason"},
		{"-30", "", "negative seconds too"},
		{"nonsense", "", "a typo must not be honoured as a window"},
		{"6h ", "", "trailing whitespace is a typo, not a duration"},
	}
	for _, c := range cases {
		t.Setenv("ORCHICON_EPHEMERAL_SWEEP_TTL", c.env)
		got := TTLFromEnv()
		if c.want == "" {
			if got != DefaultTTL {
				t.Errorf("TTLFromEnv() with env %q = %s, want the default %s (%s)", c.env, got, DefaultTTL, c.why)
			}
			continue
		}
		if got.String() != c.want {
			t.Errorf("TTLFromEnv() with env %q = %s, want %s (%s)", c.env, got, c.want, c.why)
		}
	}
	// The default has to be far longer than a real Quick Work job (a
	// conversational turn), or the sweep would race live jobs.
	if DefaultTTL < time.Hour {
		t.Errorf("DefaultTTL is %s — too close to a real job's duration for a sweep that cannot tell "+
			"\"abandoned\" from \"still running\"", DefaultTTL)
	}
}

// A sweeper built without a usable TTL must still sweep — with the default.
func TestNewSweeperFallsBackToTheDefaultTTL(t *testing.T) {
	t.Setenv("ORCHICON_EPHEMERAL_SWEEP_TTL", "garbage")
	s := NewSweeper(nil, nil)
	if s.TTL() != DefaultTTL {
		t.Errorf("NewSweeper().TTL() = %s, want the default %s", s.TTL(), DefaultTTL)
	}
}
