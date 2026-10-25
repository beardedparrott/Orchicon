package orchicon

// askid_uniqueness_test.go — an ask id must be unique ACROSS PROCESS LIVES, not just within one.
//
// THE BUG THIS PINS, read out of the production database after eleven plane restarts in a day:
//
//	native-ask-1 | ask, fullsend_approved, user_PERMISSION_CHOICE_ALLOW_ONCE, user_PERMISSION_CHOICE_ALLOW_SESSION
//	native-ask-2 | ask, fullsend, session_grant, user_PERMISSION_CHOICE_ALLOW_ONCE, …
//
// One id, incompatible histories, because the counter was per-process and restarted at zero. The transcript
// ledger is the SETTLE AUTHORITY and it is keyed by this id, so a new ask inheriting an answered ask's id
// was settled from the OLD recorded outcome and then re-armed from the open-ask list — settle → re-arm →
// settle at poll cadence, in EVERY client. That is the card flicker.

import (
	"io"
	"log/slog"
	"strings"
	"testing"
)

func quietBridge() *NativeBridge {
	return NewBridge(nil, "", slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// THE REGRESSION. Two bridges stand for two plane lives: the ids the second mints must never collide with
// the first's, because the recorded outcomes of life one are still in the ledger during life two.
func TestAskIDsAreUniqueAcrossRestarts(t *testing.T) {
	before := quietBridge()
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		seen[before.nextPermID()] = true
	}

	// A RESTART: a brand-new bridge, its counter back at zero — exactly what a rebuild does.
	after := quietBridge()
	for i := 0; i < 50; i++ {
		id := after.nextPermID()
		if seen[id] {
			t.Fatalf("ask id %q was minted again after a restart. The ledger still holds the previous ask's "+
				"recorded outcome under that id, so a NEW card would be settled from an OLD decision and "+
				"then re-armed from the open-ask list — the settle/re-arm flicker both clients showed", id)
		}
		seen[id] = true
	}
}

// And unique within one life, which is the cheaper property the old code did have — kept so a future
// change cannot trade one for the other.
func TestAskIDsAreUniqueWithinOneProcess(t *testing.T) {
	b := quietBridge()
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		id := b.nextPermID()
		if seen[id] {
			t.Fatalf("ask id %q minted twice in one process", id)
		}
		seen[id] = true
	}
}

// The prefix survives, because the log groups asks by it ("native-ask-…") and a reader looks for it.
func TestAskIDsKeepTheNativePrefix(t *testing.T) {
	id := quietBridge().nextPermID()
	if !strings.HasPrefix(id, "native-ask-") {
		t.Errorf("ask id %q lost the native-ask- prefix that the logs group by", id)
	}
	if strings.TrimPrefix(id, "native-ask-") == "" {
		t.Errorf("ask id %q has no unique component at all", id)
	}
}
