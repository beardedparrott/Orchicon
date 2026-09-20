package orchicon

// Purge-conversation-history tests (scheduler.ConversationHistoryPurger). The
// native adapter is sessionless, so its persisted history file is the ONLY
// on-disk artifact of a conversation — and until this capability existed,
// nothing reclaimed it: every deleted conversation leaked its file forever
// (observed live: 19 files / 19MB, several multi-MB).
//
// These are pure unit tests (no DB, no provider): they exercise the real
// persist path and then assert the artifact is gone and the operation is
// idempotent.

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/beardedparrott/orchicon/internal/scheduler"
)

func purgeTestStr(s string) *string { return &s }

// seedAskHistory creates a conversation session on a bridge backed by dir,
// seeds one in-memory message through the REAL persist path, and returns the
// bridge, its session id, and the path the history file was written to.
func seedAskHistory(t *testing.T, dir, convID string) (*NativeBridge, string, string) {
	t.Helper()
	b := NewBridge(nil, "", nil)
	b.SetAskHistoryDir(dir)
	sid, err := b.CreateConversationSession(context.Background(), convID, "title")
	if err != nil {
		t.Fatalf("create conversation session: %v", err)
	}
	b.mu.Lock()
	b.chatHistory[sid] = []Message{{Role: RoleUser, Content: []Content{{Text: purgeTestStr("hello")}}}}
	b.persistAskHistoryLocked(sid)
	b.mu.Unlock()
	path := filepath.Join(dir, askHistoryFilename(sid)+".json")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("history file should exist at %s: %v", path, err)
	}
	return b, sid, path
}

func TestPurgeConversationHistoryRemovesFileAndMemory(t *testing.T) {
	dir := t.TempDir()
	const convID = "01PURGETESTCONV0000000000"
	b, sid, path := seedAskHistory(t, dir, convID)

	if err := b.PurgeConversationHistory(context.Background(), convID, sid); err != nil {
		t.Fatalf("purge: %v", err)
	}

	// The persisted artifact is gone...
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("history file still present after purge (stat err = %v)", err)
	}
	// ...and the in-memory map entry no longer holds the conversation, so a
	// later turn cannot resurrect the purged history by re-persisting it.
	b.mu.Lock()
	_, still := b.chatHistory[sid]
	b.mu.Unlock()
	if still {
		t.Error("in-memory history entry survived the purge")
	}
}

func TestPurgeConversationHistoryIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	const convID = "01PURGEIDEMPOTENT00000000"
	b, sid, _ := seedAskHistory(t, dir, convID)

	for i := 1; i <= 2; i++ {
		if err := b.PurgeConversationHistory(context.Background(), convID, sid); err != nil {
			t.Fatalf("purge call %d: %v", i, err)
		}
	}
}

func TestPurgeConversationHistoryResolvesSyntheticSessionID(t *testing.T) {
	// A conversation row whose session id was never persisted still owns a
	// history file under the adapter's synthetic id — purging must resolve it
	// rather than silently no-op (which would re-leak the file).
	dir := t.TempDir()
	const convID = "01PURGENOSESSIONID00000000"
	b, sid, path := seedAskHistory(t, dir, convID)
	if sid != scheduler.NativeSessionIDPrefix+convID {
		t.Fatalf("session id = %q, want the synthetic prefix + conversation id", sid)
	}

	// Empty session id: the purge must derive it from the conversation id.
	if err := b.PurgeConversationHistory(context.Background(), convID, ""); err != nil {
		t.Fatalf("purge with empty session id: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("history file not purged when session id was empty (stat err = %v)", err)
	}
}

func TestPurgeConversationHistoryMemoryOnlyNoError(t *testing.T) {
	// No askHistoryDir configured (memory-only deployment): the purge must
	// still clear the in-memory entry and must not error on a missing file.
	b := NewBridge(nil, "", nil)
	const convID = "01PURGEMEMORYONLY00000000"
	sid, err := b.CreateConversationSession(context.Background(), convID, "t")
	if err != nil {
		t.Fatalf("create conversation session: %v", err)
	}
	b.mu.Lock()
	b.chatHistory[sid] = []Message{{Role: RoleUser, Content: []Content{{Text: purgeTestStr("x")}}}}
	b.mu.Unlock()

	if err := b.PurgeConversationHistory(context.Background(), convID, sid); err != nil {
		t.Fatalf("purge with no history dir: %v", err)
	}
	b.mu.Lock()
	_, still := b.chatHistory[sid]
	b.mu.Unlock()
	if still {
		t.Error("in-memory history entry survived a memory-only purge")
	}
}
