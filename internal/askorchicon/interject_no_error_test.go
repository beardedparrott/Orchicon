package askorchicon

// interject_no_error_test.go — the reported GUI failure, pinned.
//
// REPORTED: "It gave an error message saying the message could not be sent and would be saved, then it left the
// text in the composer, and then it looked like it sent it after the model's turn was done with a user card but
// then it didn't appear to be sent to the user… What should happen, is it should send it immediately into the
// chat, no error message, and then fit it into the context when it is possible (a turn occurs)."
//
// THE CAUSE. Interjecting delivered with SendTurnMessage against the live session. Correct for opencode, whose
// serve queues a prompt to the running turn's boundary. WRONG for the native bridge, which emulates a session:
// its SendTurnMessage starts a NEW provider turn and takes over the running turn's cancel handle, so mid-turn —
// and especially while the turn was blocked on an ask_user answer — the second stream was rejected, and the
// operator got an error for a message that should simply have waited.

import (
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/scheduler"
)

// nonQueueingClient is a ChatTurnClient that, like the native bridge, does NOT hold a prompt for a running
// turn's boundary.
type nonQueueingClient struct{ fakeSessionClient }

func (c *nonQueueingClient) QueuesPrompts() bool { return false }

func TestQueuesPromptsDefaultsToNotQueueing(t *testing.T) {
	// THE SAFE DEFAULT. An adapter that does not declare the capability must be treated as NOT queueing,
	// because the unsafe reading starts a competing turn on its session.
	var c scheduler.ChatTurnClient = &fakeSessionClient{}
	if queuesPrompts(c) {
		t.Fatal("an adapter that does not declare SessionQueuesPrompts was treated as queueing; a send " +
			"mid-turn would start a competing provider turn")
	}
}

func TestQueuesPromptsRespectsTheDeclaration(t *testing.T) {
	var q scheduler.ChatTurnClient = queueingClient{&fakeSessionClient{}}
	if !queuesPrompts(q) {
		t.Error("the opencode adapter declares it queues and was not believed")
	}
	var n scheduler.ChatTurnClient = &nonQueueingClient{}
	if queuesPrompts(n) {
		t.Error("the native bridge declares it does NOT queue and was not believed")
	}
}

// TestTheNativeBridgeDeclaresItQueues is the contract that makes an interjection JOIN the running turn,
// asserted where the behaviour lives rather than only in this package's fakes.
//
// IT USED TO ASSERT `return false`, and that was right at the time: SendTurnMessage started a provider turn,
// so delivering mid-turn ran a SECOND concurrent stream that the provider rejected. So askorchicon HELD the
// message until the next turn — the behaviour the operator reported as "it doesn't get seen until the turn is
// completely stopped".
//
// TRUE is correct now because the bridge has its own queue (askInjections): dispatchTurnMessage enqueues when a
// turn is live, and the turn loop drains it into the history re-sent on the next provider call. Asserted as a
// PAIR with the queue's existence, because the declaration alone would let a refactor delete the queue and
// leave askorchicon sending into nothing.
func TestTheNativeBridgeDeclaresItQueues(t *testing.T) {
	src := readSource(t, "../../internal/orchicon/chatturn.go")
	if !containsStr(src, "func (b *NativeBridge) QueuesPrompts() bool { return true }") {
		t.Error("the native bridge no longer declares QueuesPrompts() == true, so an interjection would be held " +
			"until the next turn instead of joining the running one — the reported \"not seen until the turn " +
			"is completely stopped\"")
	}
	// THE OTHER HALF: the declaration is only honest if the queue it promises exists and is drained.
	bridge := readSource(t, "../../internal/orchicon/bridge.go")
	if !containsStr(bridge, "askInjections map[string][]Message") {
		t.Error("QueuesPrompts() reports true but the bridge holds no injection queue — a mid-turn send would " +
			"be accepted and then lost")
	}
	if !containsStr(src, "b.drainInjections(sessionID, &working)") {
		t.Error("nothing drains the injection queue into the turn's history, so the model would never see a " +
			"queued message")
	}
}

func TestTheOpencodeAdapterDeclaresItQueues(t *testing.T) {
	src := readSource(t, "../../internal/opencode/chatsession.go")
	if !containsStr(src, "func (a *Adapter) QueuesPrompts() bool { return true }") {
		t.Error("the opencode adapter no longer declares QueuesPrompts() == true, so interjections would stop " +
			"reaching the live session even though its serve queues them properly")
	}
}

// TestAFailedInjectionIsHeldNotReported: delivery failure must not surface, and must leave the message queued
// for the next turn.
func TestAFailedInjectionIsHeldNotReported(t *testing.T) {
	s := New(nil, slog.Default(), nil, nil, nil)
	s.followUps = newFollowUpQueue()

	// What deliverInjection does when the send fails: hold it, and say nothing.
	s.queueFollowUp("conv-1", "also check the logs", nil)

	if !s.followUps.has("conv-1") {
		t.Fatal("the message was not held, so nothing will ever answer it — the operator sees a user card " +
			"with no reply, which is the reported symptom")
	}
	item, ok := s.followUps.take("conv-1")
	if !ok || item.text != "also check the logs" {
		t.Fatalf("held item = %+v ok=%v", item, ok)
	}
	if s.followUps.has("conv-1") {
		t.Error("take did not consume the item, so the follow-up would run more than once")
	}
}

// TestOnlyTheLatestInterjectionIsAnswered: an operator typing twice during one turn means the second thought
// supersedes the first as the next thing to answer.
func TestOnlyTheLatestInterjectionIsAnswered(t *testing.T) {
	s := New(nil, slog.Default(), nil, nil, nil)
	s.followUps = newFollowUpQueue()
	s.queueFollowUp("conv-1", "first thought", nil)
	s.queueFollowUp("conv-1", "second thought", nil)

	item, _ := s.followUps.take("conv-1")
	if item.text != "second thought" {
		t.Errorf("held %q, want the latest message", item.text)
	}
	// Both are still in the transcript — the hold is about what to ANSWER, not about what happened.
	if s.followUps.has("conv-1") {
		t.Error("the queue kept an item after take")
	}
}

// TestAnEmptyInjectionIsNotQueued: whitespace is not a message.
func TestAnEmptyInjectionIsNotQueued(t *testing.T) {
	s := New(nil, slog.Default(), nil, nil, nil)
	s.followUps = newFollowUpQueue()
	s.queueFollowUp("conv-1", "   ", nil)
	if s.followUps.has("conv-1") {
		t.Error("whitespace was queued as a message to answer")
	}
}

// TestAFollowUpDoesNotRequeue itself is the guard against a request loop: a follow-up that fails must leave the
// message in the transcript, not re-queue it to fail again.
func TestAFollowUpDoesNotRequeue(t *testing.T) {
	src := readSource(t, "chat.go")
	i := indexOfStr(src, "func (s *Service) startQueuedFollowUp(")
	if i < 0 {
		t.Fatal("startQueuedFollowUp is gone")
	}
	body := src[i:]
	if j := indexOfStr(body, "\nfunc "); j > 0 {
		body = body[:j]
	}
	// The ONLY put() inside it is the "a turn is already in flight, leave it for that turn" branch — never on
	// the error path.
	if containsStr(body, "could not start — the message stays in the transcript for the next turn\"") &&
		!containsStr(body, "NOT RE-QUEUED") {
		t.Error("the follow-up error path does not state that it deliberately does not re-queue")
	}
	if !containsStr(body, "s.followUps.put(convID, item.text, item.attachments)") {
		t.Error("the in-flight branch no longer leaves the message for the turn that is running")
	}
}

// readSource reads a file relative to this package, for the cross-package contract assertions.
func readSource(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

func containsStr(s, sub string) bool { return strings.Contains(s, sub) }
func indexOfStr(s, sub string) int   { return strings.Index(s, sub) }
