package askmode

// scope.go — the TURN'S CONVERSATION SCOPE as a context value, beside the mode.
//
// WHY IT LIVES HERE, WITH THE MODE, rather than in the native adapter that first stamped it. The turn's
// conversation id and its project id are read by MORE than one adapter now: the native Ask provider resolves
// the conversation's MCP scope and its file/shell anchor from them, and the claude adapter needs the same ids
// to resolve its `--mcp-config`. Those keys were unexported to their original package, so a second adapter
// could not read them — and the bridge cannot re-read the conversation row itself (it holds no *db.Pool). The
// fix is the one the mode already took: put the value on the turn's context, in the package that owns "a
// turn's facts", so every adapter reads the SAME value from the SAME stamp. One read of one row, no second
// DB round trip, and no new package.
//
// It imports nothing, matching this package's contract: the scope is two strings, and the wording of any
// failure that follows is the caller's.

import "context"

// ConversationScope is the conversation a turn belongs to: its id, and the project it is assigned to ("" when
// unassigned). The zero value means "not stamped" — see ConversationScopeFromContext.
type ConversationScope struct {
	ConversationID string
	ProjectID      string
}

// scopeKey is the unexported type of the context key, so no other package can collide with it.
type scopeKey struct{}

// WithConversationScope stamps a context with the scope a turn is running under. An empty ProjectID is a
// meaningful value ("this conversation has no project"), not a missing one — the file/shell suite and the MCP
// resolver both read it that way, exactly as the native adapter's own stamp did.
func WithConversationScope(ctx context.Context, scope ConversationScope) context.Context {
	return context.WithValue(ctx, scopeKey{}, scope)
}

// ConversationScopeFromContext reads a turn's conversation scope. The ZERO VALUE means "not stamped" — a
// caller that never stamped one, or a test driving a tool directly — mirroring ModeFromContext's tolerance:
// an unstamped context makes no claim, and a consumer that needs a non-empty id must fail loud on "" rather
// than substitute a guess.
func ConversationScopeFromContext(ctx context.Context) ConversationScope {
	if ctx == nil {
		return ConversationScope{}
	}
	s, _ := ctx.Value(scopeKey{}).(ConversationScope)
	return s
}
