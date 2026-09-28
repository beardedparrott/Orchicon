package askorchicon

// conversation_compact_notice_test.go — the durable marker a collapse leaves in the
// transcript.
//
// Compaction used to be visible only in the server log: prod's own log records a
// conversation of 2,343 messages collapsed into a summary, and the transcript the
// operator was reading said nothing about it. These tests pin the two places that
// matters — the row that IS the marker, and the seed digest that must not mistake it
// for something a party said.

import (
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/scheduler"
	"github.com/beardedparrott/orchicon/internal/tenant"
)

// TestTheSeedDigestSkipsAPlatformNotice pins the seed path's treatment of a `system`
// row.
//
// The digest's label loop only knows "user" and "assistant", so a notice that fell
// through it would be replayed to the model as the OPERATOR'S OWN WORDS — and it is
// replayed on every fresh session, which is exactly when the model has nothing else
// to go on. The notice is skipped instead: it is something the platform said ABOUT
// the conversation, not something either party said in it.
func TestTheSeedDigestSkipsAPlatformNotice(t *testing.T) {
	const notice = "Context compacted to keep this conversation inside the model's window. " +
		"compacted 2343 messages into 1 summary + 7 recent messages."
	history := []db.MessageRow{
		{ID: "m1", Role: "user", Content: "please fix the crash"},
		{ID: "m2", Role: "system", Content: notice},
		{ID: "m3", Role: "assistant", Content: "looking at it now"},
	}

	prompt := buildSystemPrompt(modeIteration, testAgentConfig(), testToolRegistry(), history, true, nil, "", "")

	if strings.Contains(prompt, "Context compacted to keep this conversation") {
		t.Fatal("the seed digest replayed a platform notice — the label loop would present it to the " +
			"model as the operator's own words")
	}
	if !strings.Contains(prompt, "please fix the crash") || !strings.Contains(prompt, "looking at it now") {
		t.Fatal("the digest dropped real conversation history while skipping the notice")
	}
}

// TestRecordCompactionNoticeWritesASystemRowDB pins the marker itself: an automatic
// collapse must land in the transcript as a `system` row that names what was lost and
// where the original went.
//
// DB-gated (ORCHICON_TEST_DSN): the row is written through the real tenant-scoped
// pool, which is the part worth testing — the content builder alone would not have
// caught a role the schema rejects or a transaction that never commits.
func TestRecordCompactionNoticeWritesASystemRowDB(t *testing.T) {
	pool := chatDBTestPool(t)
	s := New(pool, slog.Default(), nil, nil, nil)
	convID := createConversation(t, pool, "orchicon/deepseek/deepseek-flash")

	const detail = "compacted 2343 messages into 1 summary + 7 recent messages (transcript reduced from 9.3MB to 412.8KB before summarizing)"
	archive := "/var/lib/orchicon/ask-history/orchicon-ask_" + convID + ".json.compacted-20260928T004606.bak"

	ctx := tenant.WithID(context.Background(), "tnt_dev")
	if err := s.RecordCompactionNotice(ctx, scheduler.AskCompactNotice{
		ConversationID: convID,
		SessionID:      "orchicon-ask:" + convID,
		Reason:         "pressure",
		Detail:         detail,
		ArchivePath:    archive,
		TokensBefore:   964310,
	}); err != nil {
		t.Fatalf("record compaction notice: %v", err)
	}

	ttx, err := pool.BeginTenantTx(ctx, "tnt_dev")
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer ttx.Rollback(ctx)
	rows, err := db.ListMessages(ctx, ttx.Tx, "tnt_dev", convID, 10, "")
	if err != nil {
		t.Fatalf("list messages: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("messages = %d, want exactly the one notice", len(rows))
	}
	row := rows[0]
	// The schema's `system` role — not `assistant`, which both clients would render
	// as the model's own words.
	if row.Role != "system" {
		t.Fatalf("notice role = %q, want %q", row.Role, "system")
	}
	for _, want := range []string{
		"compacted 2343 messages", // what was lost, in the adapter's own numbers
		"964310",                  // the measured prompt size, never an estimate
		"preserved at " + archive, // where the original went
		"SUMMARY rather than the", // and that the detail is reduced
	} {
		if !strings.Contains(row.Content, want) {
			t.Errorf("notice content does not mention %q.\ncontent:\n%s", want, row.Content)
		}
	}
}

// TestRecordCompactionNoticeIsAdvisoryWithoutATenantDB pins the contract callers rely
// on: this is best-effort bookkeeping for an event that has ALREADY happened, so it
// reports an error rather than panicking when it cannot write — and the caller's
// choice to log-and-continue stays correct.
func TestRecordCompactionNoticeReportsAnErrorWithoutATenantDB(t *testing.T) {
	pool := chatDBTestPool(t)
	s := New(pool, slog.Default(), nil, nil, nil)

	// No tenant in the context: the notice cannot be attributed, so it must not be
	// written under a guess.
	if err := s.RecordCompactionNotice(context.Background(), scheduler.AskCompactNotice{
		ConversationID: "01NOTENANTNOTENANTNOTENANTN",
		Reason:         "pressure",
		Detail:         "compacted 10 messages",
	}); err == nil {
		t.Fatal("a notice with no tenant in context was accepted — it must report an error rather than " +
			"write into the wrong tenant")
	}
}
