package scheduler

import (
	"math/rand"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"
)

// branchRefRE is the shape every produced branch must satisfy: kebab-case
// alphanumeric slug + "-" + alnum suffix. Git refs may not contain "..",
// "@{", "\\", "~", "^", ":", "?", "*", "[", or whitespace, and may not
// begin/end with "/" or "." or end with "." — the constructor's inputs
// already rule all of those out, so this is the guard for regressions.
var branchRefRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*[a-z0-9]-[a-z0-9]{16}$`)

func TestRunSuffixUsesEntropyTail(t *testing.T) {
	// ULID layout: first 10 chars = 48-bit ms timestamp, last 16 = entropy.
	runID := "01J1234567ABCDEFGHJKLMNPQR" // 26 chars
	if got := runSuffix(runID); got != "abcdefghjklmnpqr" {
		t.Fatalf("runSuffix(%q) = %q, want the lowercased 16-char entropy tail", runID, got)
	}
}

func TestRunSuffixDefensiveShortID(t *testing.T) {
	if got := runSuffix("abc123"); got != "abc123" {
		t.Fatalf("runSuffix(short id) = %q, want the whole id lowercased", got)
	}
}

// TestBranchNamesDistinctForSameMillisecondRuns is the collision-safety
// acceptance test: two run IDs armed in the SAME millisecond share the
// first 10 (timestamp) chars but differ in entropy. The branch constructor
// must produce distinct branches for them — the whole reason the suffix is
// runID[10:] (80-bit entropy) and not the timestamp prefix.
func TestBranchNamesDistinctForSameMillisecondRuns(t *testing.T) {
	ent := ulid.Monotonic(rand.New(rand.NewSource(42)), 0)
	ts := uint64(0x0168E59FCA) // fixed "now" so both ULIDs share a timestamp
	a := ulid.MustNew(ts, ent).String()
	b := ulid.MustNew(ts, ent).String()

	if a[:10] != b[:10] {
		t.Fatalf("test precondition broken: ids do not share a timestamp prefix (%q vs %q)", a, b)
	}
	if a == b {
		t.Fatalf("test precondition broken: ids identical")
	}
	if branchNameFor("refactor-export-pipeline", a) == branchNameFor("refactor-export-pipeline", b) {
		t.Fatalf("same-millisecond runs of the same item collided: %q vs %q", a, b)
	}
}

func TestBranchNameDeterministicAndRefValid(t *testing.T) {
	runID := "01J1234567ABCDEFGHJKLMNPQR"
	want := "refactor-export-pipeline-" + strings.ToLower(runID[10:])
	got := branchNameFor("Refactor Export Pipeline!", runID)
	if got != want {
		t.Fatalf("branchNameFor = %q, want %q", got, want)
	}
	if !branchRefRE.MatchString(got) {
		t.Fatalf("branch %q is not a valid kebab-case ref", got)
	}
}

// TestBranchNameMaxLength caps the slug so the full branch stays well under
// git's 255-byte ref limit even for pathological titles.
func TestBranchNameMaxLength(t *testing.T) {
	long := strings.Repeat("a-very-long-work-item-title-component-", 10) // ~440 chars
	runID := "01J1234567ABCDEFGHJKLMNPQR"
	got := branchNameFor(long, runID)
	if len(got) >= 255 {
		t.Fatalf("branch %q is %d bytes — git refs cap at 255", got, len(got))
	}
	if !strings.HasSuffix(got, "-"+strings.ToLower(runID[10:])) {
		t.Fatalf("branch %q lost its run suffix", got)
	}
}

func TestBranchNameEmptySourceFallsBack(t *testing.T) {
	runID := "01J1234567ABCDEFGHJKLMNPQR"
	got := branchNameFor("!!!", runID)
	if !strings.HasPrefix(got, "run-") {
		t.Fatalf("branchNameFor(empty source) = %q, want a 'run-' fallback prefix", got)
	}
}

func TestParseRepoSlug(t *testing.T) {
	cases := []struct {
		name   string
		remote string
		want   string
	}{
		{"https with .git", "https://github.com/beardedparrott/Orchicon.git", "beardedparrott/Orchicon"},
		{"https no .git", "https://github.com/beardedparrott/Orchicon", "beardedparrott/Orchicon"},
		{"ssh scp-like", "git@github.com:beardedparrott/Orchicon.git", "beardedparrott/Orchicon"},
		{"ssh url form", "ssh://git@github.com/beardedparrott/Orchicon.git", "beardedparrott/Orchicon"},
		{"git protocol", "git://github.com/beardedparrott/Orchicon", "beardedparrott/Orchicon"},
		{"empty", "", ""},
		{"no owner", "https://github.com/Orchicon.git", ""},
		{"not a github url", "/local/path", ""},
		{"bare remote name", "origin", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseRepoSlug(tc.remote); got != tc.want {
				t.Fatalf("parseRepoSlug(%q) = %q, want %q", tc.remote, got, tc.want)
			}
		})
	}
}

// TestSlowPassGateSkipsWithinInterval is the dispatch-stall regression
// guard: after a slow pass has run, the gate must hold (scan takes the
// slow_skipped path) so a newly armed pending run never waits behind
// cleanup; once the interval elapses the gate must release. No DB needed —
// it exercises the real slowPassDue gate scan() calls.
func TestSlowPassGateSkipsWithinInterval(t *testing.T) {
	r := &WorktreeReconciler{}
	r.lastSlowPass = time.Now()
	if r.slowPassDue() {
		t.Fatalf("slow gate did not hold immediately after a slow pass (scan would run cleanup instead of skipping it)")
	}
	r.lastSlowPass = time.Now().Add(-2 * slowPassInterval)
	if !r.slowPassDue() {
		t.Fatalf("slow gate did not release after the interval elapsed (cleanup would starve)")
	}
	// Zero value (fresh restart): first tick runs the slow pass so
	// post-restart cleanup resumes.
	r = &WorktreeReconciler{}
	if !r.slowPassDue() {
		t.Fatalf("zero-value lastSlowPass must read as due (cleanup must resume after restart)")
	}
}

// TestSlowPassBudgetExpiry is the per-tick cost-cap guard: an already-past
// deadline must read as expired (sweep loops return early on the first
// item check and resume next tick) while a fresh slowPassBudget deadline
// must not. Exercises the real sweepBudgetExpired helper the sweeps call.
func TestSlowPassBudgetExpiry(t *testing.T) {
	if !sweepBudgetExpired(time.Now().Add(-time.Second)) {
		t.Fatalf("expired budget deadline did not read as expired")
	}
	if sweepBudgetExpired(time.Now().Add(slowPassBudget)) {
		t.Fatalf("fresh slow-pass budget read as expired")
	}
}
