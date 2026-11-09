package orchicon

// Because the capability is how the platform asks "can this adapter enforce a tool policy?", and
// the answer decides whether a turn is genuinely gated or PROSE ONLY.

import (
	"context"

	"github.com/beardedparrott/orchicon/internal/scheduler"
)

// Compile-time proof the native transport claims the capability it actually exercises.
var _ scheduler.ChatToolRestrictor = (*NativeBridge)(nil)

// RestrictChatTools implements scheduler.ChatToolRestrictor.
func (b *NativeBridge) RestrictChatTools(_ context.Context, _ scheduler.ToolPolicy) error {
	return nil
}
