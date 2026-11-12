package askorchicon

// interject_queue.go — ANSWERING an injected message when the adapter cannot hold it.

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
