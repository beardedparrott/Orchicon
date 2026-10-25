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
	"context"
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

// PERMISSION CARDS SPECIFICALLY — not questions, not a generic id helper.
//
// The operator's check was exact: "I want to make sure this also applies to permission cards because it
// was permission cards that was the issue this go round and not ask cards." Both card kinds go through
// nextPermID (chatturn.go: the permission wait raises at awaitConsentPermission, the question at
// awaitUserAnswer), so the fix covers both — and this drives the REAL permission path rather than the id
// function, so the claim is tested and not just asserted.
//
// The production evidence this pins: within one 200-message window,
//
//	native-ask-1  raised=9  decided=2     ← permission raises AND permission.user_* decisions
//	native-ask-2  raised=3  decided=1
//
// i.e. the collided ids were carrying PERMISSION verdicts, which is exactly the card that flickered.
func TestPermissionCardIDsAreUniqueAcrossRestarts(t *testing.T) {
	t.Setenv("ORCHICON_ASK_CONSENT_WAIT", "5s")

	raise := func() string {
		b, bus, cancel := newConsentTestBridge(t)
		defer cancel()
		go func() {
			_ = b.awaitConsentPermission(context.Background(), bus,
				ToolCall{ToolCallID: "tc-1", Name: "bash", ArgsJSON: `{"command":"ls"}`}, `{"command":"ls"}`)
		}()
		return waitForAsk(t, b)
	}

	first := raise()
	// A RESTART — a brand-new bridge, its counter back at zero. Under the old per-process counter this
	// returned "native-ask-1" again, for a DIFFERENT ask, and the ledger was still holding the first
	// ask's decision under that id.
	second := raise()

	if first == second {
		t.Fatalf("a PERMISSION card was minted the id %q again after a restart. The transcript ledger "+
			"still holds that id's recorded decision, so the new card is settled from the OLD one and "+
			"then re-armed from the open-ask list — the permission-card flicker", first)
	}
	if !strings.HasPrefix(first, "native-ask-") {
		t.Errorf("permission ask id %q lost the native-ask- prefix", first)
	}
}
