package askorchicon

// fullsend.go — FULLSEND: the operator's per-conversation waiver of the permission PROMPT.

import (
	"sync"
)

// fullsendStore records which conversations are in FULLSEND.
type fullsendStore struct {
	mu     sync.Mutex
	byConv map[string]bool
}

func newFullsendStore() *fullsendStore {
	return &fullsendStore{byConv: make(map[string]bool)}
}

// Set turns FULLSEND on or off for one conversation.
func (f *fullsendStore) Set(convID string, on bool) {
	if f == nil || convID == "" {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if !on {
		delete(f.byConv, convID)
		return
	}
	f.byConv[convID] = true
}

// Enabled reports whether FULLSEND is on for convID.
func (f *fullsendStore) Enabled(convID string) bool {
	if f == nil || convID == "" {
		return false
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.byConv[convID]
}

// ClearConversation drops the flag for a conversation (conversation end), mirroring
// grantStore.ClearConversation.
func (f *fullsendStore) ClearConversation(convID string) {
	if f == nil || convID == "" {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.byConv, convID)
}
