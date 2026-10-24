package askorchicon

// interject_queue.go — ANSWERING an injected message when the adapter cannot hold it.
//
// THE REPORTED FAILURE. In the GUI, interjecting on a running turn produced: an error banner saying the
// message could not be sent, the draft restored into the composer, and then — once the model's turn finished —
// a user card in the transcript with no reply. The operator: "What should happen, is it should send it
// immediately into the chat, no error message, and then fit it into the context when it is possible (a turn
// occurs). None of that happened the way it should."
//
// WHY IT HAPPENED. Interjecting delivered the message with SendTurnMessage against the live session. That is
// right for opencode, whose serve queues a prompt to the running turn's next boundary. It is WRONG for the
// native bridge, which emulates a session in memory: its SendTurnMessage starts a whole new provider turn and
// takes over the running turn's cancel handle. Mid-turn — and especially while the turn was blocked on an
// ask_user answer — the provider rejected that second stream, so the send errored.
//
// WHAT HAPPENS NOW. The message is persisted immediately (it IS in the chat, at once), and the adapter is asked
// whether it can QUEUE (scheduler.SessionQueuesPrompts):
//
//   - it can (opencode)  → send it to the live session; the serve answers it at that turn's boundary;
//   - it cannot (native) → hold it here, and answer it with a follow-up turn the moment the running turn
//     finishes. That is literally "fit it into the context when it is possible (a turn occurs)".
//
// The hold is per conversation and keeps only the LATEST message: an operator typing twice during one turn
// means the second thought supersedes the first as the next thing to answer, and replaying both would answer a
// message they have already moved past. Nothing is lost from the transcript either way — both messages are
// persisted rows.
//
// THE FLAG IS CONSUMED ONCE. A follow-up that itself fails does not re-queue itself (that is how a
// request-and-fail loop gets built); the message stays in the transcript and the next operator turn carries it,
// because the native bridge re-sends the session history it holds.

import (
	"strings"
	"sync"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
)

// pendingFollowUp is one persisted user message waiting for a turn to answer it.
type pendingFollowUp struct {
	text        string
	attachments []*apiv1.AttachmentInput
}

// followUpQueue holds at most one pending message per conversation.
type followUpQueue struct {
	mu    sync.Mutex
	items map[string]pendingFollowUp
}

func newFollowUpQueue() *followUpQueue {
	return &followUpQueue{items: map[string]pendingFollowUp{}}
}

// put records the message a finished turn should answer.
func (q *followUpQueue) put(convID, text string, attachments []*apiv1.AttachmentInput) {
	if strings.TrimSpace(text) == "" {
		return
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	q.items[convID] = pendingFollowUp{text: text, attachments: attachments}
}

// take removes and returns the pending message, if any. CONSUMING IT is what makes the follow-up exactly once.
func (q *followUpQueue) take(convID string) (pendingFollowUp, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	it, ok := q.items[convID]
	if ok {
		delete(q.items, convID)
	}
	return it, ok
}

// has reports whether a message is waiting (used to keep a finishing turn from forgetting one).
func (q *followUpQueue) has(convID string) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	_, ok := q.items[convID]
	return ok
}

// queueFollowUp records an injected message to be answered by the next turn.
func (s *Service) queueFollowUp(convID, text string, attachments []*apiv1.AttachmentInput) {
	if s.followUps == nil {
		return
	}
	s.followUps.put(convID, text, attachments)
}
