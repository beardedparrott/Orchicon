package askorchicon

// fullsend.go — FULLSEND: the operator's per-conversation waiver of the permission
// PROMPT.
//
// WHAT IT IS FOR. The consent card is a gate, and a gate that cannot be opened
// deliberately gets bypassed accidentally: the operator, mid-task, approving card
// after card for the same work, stops reading them. FULLSEND is the honest version of
// that — one explicit, loudly-indicated, revocable state in which the gate does not
// open at all.
//
// WHAT IT WAIVES IS THE PROMPT, NOT THE POLICY. Two things outrank it, and both were
// designed that way before it existed:
//
//   - A DENY entry. A deny is a DECISION the policy already made — no card is ever
//     raised for it — so there is nothing here to waive. The preset denials
//     (~/.ssh, ~/.gnupg, ~/.aws, ~/.config/gh, ~/.git-credentials, ~/.netrc,
//     ~/.docker/config.json) and anything the operator writes keep refusing.
//   - The never-allow binary class (sudo / dd / mkfs* / fdisk / parted / shred /
//     wipefs / LVM / mkswap), which is refused BEFORE any permission decision is
//     reached: see binaryClassRefusal, called first in consentTurn.decide.
//
// The distinction is not decoration. "FULLSEND" that quietly opened ~/.ssh would be a
// mode whose name promises more than it does in a direction that matters, and one the
// operator could not reason about.
//
// LIFETIME AND SCOPE: per conversation, in memory, dying with the plane — the same
// scope and lifetime as a session grant (grantStore), and for the same reason. A
// permission bypass that survives a restart is one the operator has forgotten is on,
// so the state is never persisted and a fresh plane starts every conversation OFF.

import (
	"sync"
)

// fullsendStore records which conversations are in FULLSEND.
//
// Keyed by conversation id, which is tenant-scoped, so a cross-tenant conversation
// cannot be addressed — the same argument the grant and pending stores rest on.
type fullsendStore struct {
	mu     sync.Mutex
	byConv map[string]bool
}

func newFullsendStore() *fullsendStore {
	return &fullsendStore{byConv: make(map[string]bool)}
}

// Set turns FULLSEND on or off for one conversation. Setting OFF DELETES the entry
// rather than storing false, so the map only ever holds conversations that are ON and
// Enabled's fast path is a real question.
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

// Enabled reports whether FULLSEND is on for convID. A nil store, an empty id and an
// unknown conversation are all OFF — the fail-closed direction, so a store that was
// never initialised cannot silently approve everything.
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
