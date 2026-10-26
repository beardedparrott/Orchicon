package runtimeimage

// ordering_and_presence_test.go — the two rules behind the runtime-image reports.
//
// REPORTED together: "images are downloading now, however, they fail unless you download the base image
// first. I think we should prevent the download of the other images until the base is downloaded first
// and say so" and "When I tried to deploy orchicon-dev and the base image, I got 'runtime image already
// up to date (spec version 1) — skipping rebuild' but it just says stock and the deploy button is still
// lit up and clickable."

import (
	"context"
	"errors"
	"strings"
	"testing"

	"connectrpc.com/connect"

	"github.com/beardedparrott/orchicon/internal/db"
)

// newPresenceService builds a Service whose presence probe is faked. base is what ensureBase will
// report, so a test never needs a daemon to name one.
func newPresenceService(base string, present map[string]bool, err error) *Service {
	s := &Service{base: base}
	s.presentFn = func(_ context.Context, ref string) (bool, error) {
		if err != nil {
			return false, err
		}
		return present[ref], nil
	}
	// NO rt AND A SEAM: the probe goes through presentFn, and ensureBase returns the base this helper
	// was given. A zero *runtime.Client here would dereference its own nil internals the first time a
	// test asked it for anything — which is exactly what happened when this helper first set one.
	return s
}

// TestAVariantCannotBeFetchedBeforeTheBase pins the ordering rule and its MESSAGE.
//
// The dependency is structural: every variant's Dockerfile derives FROM the base, so fetching one first
// can only fail — and it fails late inside the engine with an error about a tag rather than about the
// thing the operator has to do:
//
//	creating build container: short-name "orchicon-runtime:local" did not resolve to an alias
//
// Refusing up front is the difference between that and one sentence naming the action.
func TestAVariantCannotBeFetchedBeforeTheBase(t *testing.T) {
	const base = "ghcr.io/beardedparrott/orchicon-runtime:latest"
	const variant = "ghcr.io/beardedparrott/orchicon-runtime:dev"

	s := newPresenceService(base, map[string]bool{base: false}, nil)
	err := s.requireBasePresent(context.Background(), db.RuntimeImageRow{Tag: variant})
	if err == nil {
		t.Fatal("a variant was allowed before the base — the fetch will fail late and confusingly")
	}
	var cerr *connect.Error
	if !errors.As(err, &cerr) || cerr.Code() != connect.CodeFailedPrecondition {
		t.Errorf("code = %v, want FailedPrecondition — the caller must change what they do, this is "+
			"not an internal fault", err)
	}
	// IT MUST SAY BOTH THINGS: which base is missing, and that it comes first.
	if !strings.Contains(err.Error(), base) {
		t.Errorf("the message does not name the base that is missing: %v", err)
	}
	if !strings.Contains(strings.ToLower(err.Error()), "first") {
		t.Errorf("the message does not say the base comes FIRST — that is the whole instruction: %v", err)
	}
}

// TestTheBaseIsExemptFromItsOwnRule — without this the rule would make fetching impossible.
func TestTheBaseIsExemptFromItsOwnRule(t *testing.T) {
	const base = "ghcr.io/beardedparrott/orchicon-runtime:latest"
	s := newPresenceService(base, map[string]bool{base: false}, nil)
	if err := s.requireBasePresent(context.Background(), db.RuntimeImageRow{Tag: base}); err != nil {
		t.Errorf("the base was refused by its own rule: %v", err)
	}
}

// TestAVariantIsAllowedOnceTheBaseExists — the rule must not block the normal path.
func TestAVariantIsAllowedOnceTheBaseExists(t *testing.T) {
	const base = "ghcr.io/beardedparrott/orchicon-runtime:latest"
	s := newPresenceService(base, map[string]bool{base: true}, nil)
	if err := s.requireBasePresent(context.Background(), db.RuntimeImageRow{Tag: "x:y"}); err != nil {
		t.Errorf("a variant was refused with the base present: %v", err)
	}
}

// TestAnUnknownBaseIsNotBlocked — a daemon that cannot name its base must not stop every fetch; the
// build reports its own failure in that case, which is a better message than a guess.
func TestAnUnknownBaseIsNotBlocked(t *testing.T) {
	s := newPresenceService("", map[string]bool{}, nil)
	if err := s.requireBasePresent(context.Background(), db.RuntimeImageRow{Tag: "x:y"}); err != nil {
		t.Errorf("an unnamed base blocked a fetch: %v", err)
	}
}

// TestAPresenceProbeFailureDoesNotBlock — "cannot tell" is not "absent". A probe error must fall
// through to the build (which reports a real reason) rather than inventing a base-missing story.
func TestAPresenceProbeFailureDoesNotBlock(t *testing.T) {
	s := newPresenceService("base:1", nil, errors.New("daemon unreachable"))
	if err := s.requireBasePresent(context.Background(), db.RuntimeImageRow{Tag: "x:y"}); err != nil {
		t.Errorf("a probe failure was reported as a missing base: %v", err)
	}
}

// TestADeploySkipsOnlyWhenTheImageIsReallyThere is the "already up to date" report, as a table.
//
// The row and the engine are two different sources of truth, and the whole defect was consulting only
// the first. Each row below is a case where they disagree.
func TestADeploySkipsOnlyWhenTheImageIsReallyThere(t *testing.T) {
	for _, tc := range []struct {
		name        string
		dbSaysBuilt bool
		present     bool
		probeErr    error
		wantSkip    bool
		why         string
	}{
		{"row built and image present", true, true, nil, true, "the only case where skipping is honest"},
		{"row built but image ABSENT", true, false, nil, false,
			"the reported bug: 'already up to date — skipping rebuild' with nothing on the host"},
		{"row built but the probe failed", true, false, errors.New("daemon down"), false,
			"cannot tell must try the build, never claim success"},
		{"row not built", false, true, nil, false, "a spec edit must rebuild even if a stale image exists"},
	} {
		got, why := alreadyBuilt(tc.dbSaysBuilt, tc.present, tc.probeErr)
		if got != tc.wantSkip {
			t.Errorf("%s: skip = %v, want %v (%s)", tc.name, got, tc.wantSkip, tc.why)
		}
		if !got && why == "" {
			t.Errorf("%s: a refusal must carry a reason for the log line", tc.name)
		}
	}
}
