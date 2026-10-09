package runtime

import (
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

// sweepHarness is a fake docker that records every invocation, so a test can
// assert both WHAT was listed (the instance filter) and WHAT was removed.
// The filter is the whole point: an instance-scoped sweep and a host-wide sweep
// issue the same `rm -f`, and only the filter tells them apart.
type sweepHarness struct {
	mu      sync.Mutex
	psCalls [][]string
	removed []string
	listed  []string
}

func (h *sweepHarness) docker(args ...string) (string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(args) > 0 && args[0] == "ps" {
		h.psCalls = append(h.psCalls, append([]string(nil), args...))
		return strings.Join(h.listed, "\n") + "\n", nil
	}
	if len(args) >= 3 && args[0] == "rm" && args[1] == "-f" {
		h.removed = append(h.removed, args[2])
		return "", nil
	}
	return "", nil
}

func (h *sweepHarness) psArgs() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.psCalls) == 0 {
		return nil
	}
	return h.psCalls[len(h.psCalls)-1]
}

func (h *sweepHarness) psCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.psCalls)
}

func (h *sweepHarness) removedNames() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.removed...)
}

func newSweepPool(t *testing.T, listed ...string) (*daemonPool, *sweepHarness) {
	t.Helper()
	h := &sweepHarness{listed: listed}
	d := &Daemon{
		Log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		dockerFn: h.docker,
	}
	return newDaemonPool(d), h
}

// TestResetPoolTouchesNoDocker is the regression for the cross-instance hazard.
//
// resetPool used to call listRuntimes("") and `rm -f` every orchicon.workflow
// container on the HOST, so starting the daemon for one instance destroyed every
// instance's containers — including a sibling's live one mid-run. It must now
// touch no container at all.
func TestResetPoolTouchesNoDocker(t *testing.T) {
	p, h := newSweepPool(t, "orchicon-runtime-aaa-111", "orchicon-runtime-bbb-222")
	p.resetPool()

	if n := h.psCount(); n != 0 {
		t.Errorf("resetPool listed containers (%d ps calls) — it must not touch docker", n)
	}
	if r := h.removedNames(); len(r) != 0 {
		t.Errorf("resetPool REMOVED %v — a daemon start must delete no container (that is the hazard)", r)
	}
}

// TestSweepIsInstanceScoped pins that the reconciliation names its instance, so
// it cannot reach another instance's containers.
func TestSweepIsInstanceScoped(t *testing.T) {
	p, h := newSweepPool(t)
	p.sweepPreRestartOrphans("dev")

	args := h.psArgs()
	if args == nil {
		t.Fatal("the sweep must list the instance's containers")
	}
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "label=orchicon.instance=dev") {
		t.Errorf("the sweep must filter by instance; got: %s", joined)
	}
	// An EMPTY filter would mean every instance on the host — the hazard. Checked
	// per argument rather than by suffix, because `--format` trails the filter and
	// a suffix test could never see it.
	for i, a := range args {
		if a == "label=orchicon.instance=" || (a == "--filter" && i+1 < len(args) && args[i+1] == "label=orchicon.instance=") {
			t.Errorf("the sweep used an EMPTY instance filter — that is the host-wide sweep; got: %s", joined)
		}
	}
	if !strings.Contains(joined, "label=orchicon.workflow") {
		t.Errorf("the sweep must still scope to runtime containers; got: %s", joined)
	}
}

// TestSweepRefusesEmptyInstance is the guard against reintroducing the hazard: an
// empty instance means EVERY instance, so it must do nothing rather than
// everything.
func TestSweepRefusesEmptyInstance(t *testing.T) {
	p, h := newSweepPool(t, "orchicon-runtime-aaa-111")
	p.sweepPreRestartOrphans("")

	if n := h.psCount(); n != 0 {
		t.Errorf("an empty instance listed containers (%d ps calls) — empty means every instance", n)
	}
	if r := h.removedNames(); len(r) != 0 {
		t.Errorf("an empty-instance sweep removed %v — that is the cross-instance hazard", r)
	}
}

// TestSweepReapsUntrackedOnly pins the discrimination: a container the pool
// still tracks belongs to a live lease and must survive, while an untracked one
// is this instance's pre-restart orphan.
func TestSweepReapsUntrackedOnly(t *testing.T) {
	live := "orchicon-runtime-live-111"
	stale := "orchicon-runtime-stale-222"
	p, h := newSweepPool(t, live, stale)

	p.mu.Lock()
	p.entries[live] = &poolEntry{name: live, lastUsed: time.Now()}
	p.mu.Unlock()

	p.sweepPreRestartOrphans("dev")

	removed := h.removedNames()
	if len(removed) != 1 || removed[0] != stale {
		t.Fatalf("removed = %v, want exactly [%s] — a tracked container must survive", removed, stale)
	}
}

// TestSweepRunsOncePerInstance pins both halves of the claim: it is not repeated
// for the same instance, and it is NOT global — a second instance gets its own
// first sweep rather than being starved by the first.
func TestSweepRunsOncePerInstance(t *testing.T) {
	p, h := newSweepPool(t)

	p.sweepPreRestartOrphans("dev")
	if n := h.psCount(); n != 1 {
		t.Fatalf("dev's first sweep listed %d times, want 1", n)
	}
	p.sweepPreRestartOrphans("dev")
	if n := h.psCount(); n != 1 {
		t.Errorf("a repeat sweep for the same instance listed again (%d) — it must be once per instance", n)
	}

	p.sweepPreRestartOrphans("prod")
	if n := h.psCount(); n != 2 {
		t.Errorf("prod's first sweep was skipped (%d listings) — the claim must be PER INSTANCE, not global", n)
	}
}

// TestSweepRetriesAfterAFailedListing pins that a transient docker failure does
// not spend the one claim: the cleanup is deferred to the next lease rather than
// lost for the daemon's whole lifetime.
func TestSweepRetriesAfterAFailedListing(t *testing.T) {
	var mu sync.Mutex
	fail := true
	var removals []string
	d := &Daemon{
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		dockerFn: func(args ...string) (string, error) {
			mu.Lock()
			defer mu.Unlock()
			if len(args) > 0 && args[0] == "ps" {
				if fail {
					return "", errFakeDocker
				}
				return "orchicon-runtime-stale-222\n", nil
			}
			if len(args) >= 3 && args[0] == "rm" && args[1] == "-f" {
				removals = append(removals, args[2])
			}
			return "", nil
		},
	}
	p := newDaemonPool(d)

	p.sweepPreRestartOrphans("dev") // listing fails: no removal, and no claim spent
	if got := len(removals); got != 0 {
		t.Fatalf("a failed listing removed %d containers, want 0", got)
	}

	mu.Lock()
	fail = false
	mu.Unlock()

	p.sweepPreRestartOrphans("dev") // now it must actually run
	if len(removals) != 1 || removals[0] != "orchicon-runtime-stale-222" {
		t.Fatalf("after the retry removals = %v, want [orchicon-runtime-stale-222]", removals)
	}
}

type fakeDockerErr struct{}

func (fakeDockerErr) Error() string { return "docker unavailable" }

var errFakeDocker = fakeDockerErr{}
