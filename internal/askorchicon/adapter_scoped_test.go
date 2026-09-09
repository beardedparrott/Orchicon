package askorchicon

import (
	"context"
	"testing"

	"github.com/beardedparrott/orchicon/internal/opencode"
	"github.com/beardedparrott/orchicon/internal/orchicon"
	"github.com/beardedparrott/orchicon/internal/scheduler"
)

// ownerKindClient is a minimal scheduler.ChatTurnClient stub whose only job
// is to report a SessionOwnerKind (it is never driven — adapterScopedSessionID
// only type-asserts it for SessionOwnerKind).
type ownerKindClient struct {
	kind string
}

func (c *ownerKindClient) Subscribe(ctx context.Context, conversationID string) (scheduler.SessionBus, error) {
	return nil, nil
}
func (c *ownerKindClient) CreateConversationSession(ctx context.Context, conversationID, title string) (string, error) {
	return "", nil
}
func (c *ownerKindClient) SendTurnMessage(ctx context.Context, conversationID, sessionID, system, modelRef, text string) error {
	return nil
}
func (c *ownerKindClient) AbortConversationSession(ctx context.Context, sessionID string) error {
	return nil
}
func (c *ownerKindClient) ReplyPermission(ctx context.Context, sessionID, permissionID string) error {
	return nil
}
func (c *ownerKindClient) SessionOwnerKind() string { return c.kind }

// noOwnerClient implements scheduler.ChatTurnClient but NOT
// scheduler.SessionOwnerKind — the "cannot resolve ownership" case.
type noOwnerClient struct{}

func (c *noOwnerClient) Subscribe(ctx context.Context, conversationID string) (scheduler.SessionBus, error) {
	return nil, nil
}
func (c *noOwnerClient) CreateConversationSession(ctx context.Context, conversationID, title string) (string, error) {
	return "", nil
}
func (c *noOwnerClient) SendTurnMessage(ctx context.Context, conversationID, sessionID, system, modelRef, text string) error {
	return nil
}
func (c *noOwnerClient) AbortConversationSession(ctx context.Context, sessionID string) error {
	return nil
}
func (c *noOwnerClient) ReplyPermission(ctx context.Context, sessionID, permissionID string) error {
	return nil
}

func TestAdapterScopedSessionID(t *testing.T) {
	s := &Service{}
	const nativeSID = "orchicon-ask:conversation-1"
	const opencodeSID = "opencode-session-abc123"

	cases := []struct {
		name   string
		client scheduler.ChatTurnClient
		sid    string
		want   string
	}{
		{
			name:   "empty sid stays empty",
			client: &ownerKindClient{kind: "opencode"},
		},
		{
			name:   "native adapter keeps the opencode id (reverse direction)",
			client: &ownerKindClient{kind: "orchicon"},
			sid:    opencodeSID,
			want:   opencodeSID,
		},
		{
			name:   "native adapter keeps the native id",
			client: &ownerKindClient{kind: "orchicon"},
			sid:    nativeSID,
			want:   nativeSID,
		},
		{
			name:   "opencode adapter clears native synthetic id (cross-adapter leak)",
			client: &ownerKindClient{kind: "opencode"},
			sid:    nativeSID,
			want:   "",
		},
		{
			name:   "opencode adapter keeps an opencode id",
			client: &ownerKindClient{kind: "opencode"},
			sid:    opencodeSID,
			want:   opencodeSID,
		},
		{
			name:   "no-owner client clears a native id",
			client: &noOwnerClient{},
			sid:    nativeSID,
			want:   "",
		},
		{
			name:   "no-owner client keeps a non-native id",
			client: &noOwnerClient{},
			sid:    opencodeSID,
			want:   opencodeSID,
		},
	}
	for i := range cases {
		tc := &cases[i]
		t.Run(tc.name, func(t *testing.T) {
			got := s.adapterScopedSessionID(tc.client, tc.sid)
			if got != tc.want {
				t.Fatalf("adapterScopedSessionID(%q) = %q, want %q", tc.sid, got, tc.want)
			}
		})
	}
}

// Compile-time pins: the production adapters must report their owner kind so
// the cross-adapter turn path recognizes them (their Kind/SessionOwnerKind
// must not drift without these breaking).
func TestProductionAdaptersReportSessionOwnerKind(t *testing.T) {
	for _, c := range []interface{ SessionOwnerKindCompile() }{nil} {
		_ = c
	}
	// NativeBridge and opencode.Adapter both implement SessionOwnerKind.
	var _ scheduler.SessionOwnerKind = (*orchicon.NativeBridge)(nil)
	var _ scheduler.SessionOwnerKind = (*opencode.Adapter)(nil)
}
