package orchicon

// askhistory_storage_test.go — the persistence cap must never mean "write
// nothing".
//
// The behaviour under test is a cliff, not a cap. persistAskHistoryLocked used to
// return early, writing NOTHING, the moment a session's serialized history passed
// askHistoryMaxBytes. The file on disk then froze at the last snapshot that fit, so
// every later turn was persisted nowhere and the next restart reloaded a stale
// transcript — or, if the history never fit at all, reloaded nothing and left the
// model on the system prompt's short history digest alone. The tests below pin the
// three shapes a history can now be written in, and the one that matters most:
// whatever its size, a persisted history is still there after a restart.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// storageTestHistory builds a history of n messages, each carrying size bytes of
// text. Compressible on purpose: it lets one test drive the history past the byte
// cap (so the plain-JSON write is refused) while still fitting once compressed,
// which is the case where NOTHING may be lost.
func storageTestHistory(n, size int) []Message {
	body := strings.Repeat("a", size)
	out := make([]Message, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, Message{Role: RoleUser, Content: []Content{{Text: &body}}})
	}
	return out
}

// persistentBridge returns a bridge backed by dir with one seeded session, plus
// that session's id and the file its history is written to.
func persistentBridge(t *testing.T, dir string, history []Message) (*NativeBridge, string, string) {
	t.Helper()
	b := NewBridge(nil, "", nil)
	b.SetAskHistoryDir(dir)
	sid, err := b.CreateConversationSession(context.Background(), "01STORAGETESTCONV00000000", "t")
	if err != nil {
		t.Fatalf("create conversation session: %v", err)
	}
	b.mu.Lock()
	b.chatHistory[sid] = history
	b.persistAskHistoryLocked(sid)
	b.mu.Unlock()
	return b, sid, filepath.Join(dir, askHistoryFilename(sid)+".json")
}

// TestAPersistedHistoryThatFitsStaysPlainJSON pins the format an ordinary
// conversation still gets. It is what every earlier release wrote and can read, so
// keeping it means a rollback loses nothing — the compression below is only ever
// reached by a history that would otherwise not be written at all.
func TestAPersistedHistoryThatFitsStaysPlainJSON(t *testing.T) {
	dir := t.TempDir()
	_, _, path := persistentBridge(t, dir, storageTestHistory(4, 256))

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("no history file: %v", err)
	}
	if len(raw) >= 2 && raw[0] == 0x1f && raw[1] == 0x8b {
		t.Fatal("a history that fits was written compressed — it must stay plain JSON so an " +
			"earlier binary can still read it")
	}
	var env struct {
		Version  int       `json:"version"`
		Messages []Message `json:"messages"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("a history that fits was not readable plain JSON: %v", err)
	}
	if env.Version != askHistoryVersion {
		t.Fatalf("envelope version = %d, want %d", env.Version, askHistoryVersion)
	}
	if len(env.Messages) != 4 {
		t.Fatalf("persisted %d messages, want 4", len(env.Messages))
	}
}

// TestAnOversizeHistoryIsStillPersistedAndSurvivesARestart is the regression the
// cap used to cause. The history's JSON is over askHistoryMaxBytes, which is
// exactly the input that previously wrote no file at all.
func TestAnOversizeHistoryIsStillPersistedAndSurvivesARestart(t *testing.T) {
	dir := t.TempDir()
	// ~20MB of JSON: comfortably over the 16MB cap, and small once compressed.
	history := storageTestHistory(200, 100_000)

	if raw, err := marshalAskHistory(history); err != nil {
		t.Fatalf("marshal seed history: %v", err)
	} else if len(raw) <= askHistoryMaxBytes {
		t.Fatalf("seed history is only %d bytes — this test needs it over the %d cap",
			len(raw), askHistoryMaxBytes)
	}

	_, sid, path := persistentBridge(t, dir, history)

	if _, err := os.Stat(path); err != nil {
		t.Fatalf("an oversize history wrote no file at all (%v) — the persistence cap must never "+
			"mean writing nothing, because the file on disk then freezes at a stale snapshot and a "+
			"restart silently reloads it", err)
	}

	// A restart: a fresh bridge over the same history dir reseeds from disk.
	restarted := NewBridge(nil, "", nil)
	restarted.SetAskHistoryDir(dir)
	got := restarted.loadAskHistoryLocked(sid)
	if len(got) != len(history) {
		t.Fatalf("a restart reloaded %d of %d messages — compression fits this history whole, so "+
			"nothing about it may be lost", len(got), len(history))
	}
	if got[0].Content[0].Text == nil || *got[0].Content[0].Text != *history[0].Content[0].Text {
		t.Fatal("the reloaded history's first message does not match what was persisted")
	}
}

// TestAPlainJSONHistoryFromAnEarlierReleaseStillLoads pins the read side against
// the format the previous release wrote: a history file must survive the upgrade.
func TestAPlainJSONHistoryFromAnEarlierReleaseStillLoads(t *testing.T) {
	dir := t.TempDir()
	const convID = "01LEGACYPLAINCONV000000000"
	sid := "orchicon-ask:" + convID

	legacy := []Message{{Role: RoleUser, Content: []Content{{Text: strPtr("from an older binary")}}}}
	raw, err := json.Marshal(map[string]any{"version": askHistoryVersion, "messages": legacy})
	if err != nil {
		t.Fatalf("marshal legacy envelope: %v", err)
	}
	path := filepath.Join(dir, askHistoryFilename(sid)+".json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("write legacy file: %v", err)
	}

	b := NewBridge(nil, "", nil)
	b.SetAskHistoryDir(dir)
	got := b.loadAskHistoryLocked(sid)
	if len(got) != 1 || got[0].Content[0].Text == nil || *got[0].Content[0].Text != "from an older binary" {
		t.Fatalf("a plain-JSON history written by an earlier release did not load: %+v", got)
	}
}

// TestGzipRoundTripsThroughTheStoragePath pins the compressed framing directly,
// so the format is covered even if the trim path below it is never reached.
func TestGzipRoundTripsThroughTheStoragePath(t *testing.T) {
	raw := []byte(`{"version":1,"messages":[{"Role":"user","Content":[{"Text":"hi"}]}]}`)
	gz, err := gzipAskHistory(raw)
	if err != nil {
		t.Fatalf("gzipAskHistory: %v", err)
	}
	if len(gz) < 2 || gz[0] != 0x1f || gz[1] != 0x8b {
		t.Fatal("gzipAskHistory did not produce gzip framing — the load path detects it by magic bytes")
	}
	back, err := gunzipAskHistory(gz)
	if err != nil {
		t.Fatalf("gunzipAskHistory: %v", err)
	}
	if string(back) != string(raw) {
		t.Fatalf("round trip changed the content: got %q want %q", back, raw)
	}
}

// TestTrimAskHistoryImagesReplacesPayloadsWithoutMutatingTheLiveHistory covers the
// last-resort reduction: the image payloads must be replaced by an explicit marker
// (so the surviving transcript still says an image was there) and the LIVE history
// must keep its images, because the reduction is a storage decision, not a
// conversation one.
func TestTrimAskHistoryImagesReplacesPayloadsWithoutMutatingTheLiveHistory(t *testing.T) {
	live := []Message{
		{Role: RoleUser, Content: []Content{{Image: strPtr("data:image/png;base64,AAAA")}}},
		{Role: RoleUser, Content: []Content{{Text: strPtr("no image here")}}},
		{Role: RoleUser, Content: []Content{
			{Text: strPtr("mixed")},
			{Image: strPtr("data:image/jpeg;base64,BBBB")},
		}},
	}

	reduced, dropped := trimAskHistoryImages(live)
	if dropped != 2 {
		t.Fatalf("dropped %d images, want 2", dropped)
	}
	if reduced[0].Content[0].Image != nil {
		t.Fatal("the reduced copy still carries an image pointer")
	}
	if reduced[0].Content[0].Text == nil || *reduced[0].Content[0].Text != askHistoryImageDroppedMarker {
		t.Fatal("a dropped image must leave an explicit marker behind, never a silent hole")
	}
	if reduced[2].Content[1].Image != nil || reduced[2].Content[0].Text == nil || *reduced[2].Content[0].Text != "mixed" {
		t.Fatal("reduction dropped more of a message than the image it had to replace")
	}
	// The live history is untouched: the same message objects, images intact.
	if live[0].Content[0].Image == nil || live[2].Content[1].Image == nil {
		t.Fatal("trimAskHistoryImages mutated the live in-memory history")
	}
}
