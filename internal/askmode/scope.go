package askmode

// scope.go — the TURN'S CONVERSATION SCOPE as a context value, beside the mode.

import "context"

// ConversationScope is the conversation a turn belongs to: its id, and the project it is assigned to ("" when
// unassigned). The zero value means "not stamped" — see ConversationScopeFromContext.
type ConversationScope struct {
	ConversationID string
	ProjectID      string
}

// scopeKey is the unexported type of the context key, so no other package can collide with it.
type scopeKey struct{}

// WithConversationScope stamps a context with the scope a turn is running under.
func WithConversationScope(ctx context.Context, scope ConversationScope) context.Context {
	return context.WithValue(ctx, scopeKey{}, scope)
}

// ConversationScopeFromContext reads a turn's conversation scope.
func ConversationScopeFromContext(ctx context.Context) ConversationScope {
	if ctx == nil {
		return ConversationScope{}
	}
	s, _ := ctx.Value(scopeKey{}).(ConversationScope)
	return s
}
