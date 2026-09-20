package orchicon

// askrestrict.go — the native transport's declaration that it ENFORCES the Ask mode policy.
//
// The RESTRICTION ITSELF is not implemented here, and that is the point rather than an omission. The native
// transport owns its tool loop: it is the layer that offers the tools (NativeBridge.askToolsLocked → the
// injected provider's AskToolDefs) and the layer that runs them (ExecuteAskTool), and askorchicon's provider
// applies internal/askmode at BOTH of those points — the denied tools are never offered and a call is refused if
// one arrives anyway. There is no second mechanism to configure, because the adapter IS the mechanism — which is
// not true of an adapter that drives its own external process.
//
// SO WHY DECLARE IT AT ALL? Because the capability is how the platform asks "can this adapter enforce a tool
// policy?", and the answer decides whether a turn is genuinely gated or PROSE ONLY. Without this method the
// native transport would look exactly like an adapter that cannot enforce anything, and askorchicon would report
// the boundary as advisory on the very path where it is strongest. The method is the declaration; its emptiness
// is accurate.

import (
	"context"

	"github.com/beardedparrott/orchicon/internal/scheduler"
)

// Compile-time proof the native transport claims the capability it actually exercises.
var _ scheduler.ChatToolRestrictor = (*NativeBridge)(nil)

// RestrictChatTools implements scheduler.ChatToolRestrictor.
//
// A no-op, deliberately and documented: the native provider applies the policy at the tool layer (it is both the
// lister and the executor), so there is nothing for the bridge to configure. If a native path ever grows a
// configurable tool surface that is NOT the provider, this is where the policy would be applied — and the
// signature is already the one it would need.
func (b *NativeBridge) RestrictChatTools(_ context.Context, _ scheduler.ToolPolicy) error {
	return nil
}
