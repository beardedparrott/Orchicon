// AC8 probe: drive ProviderService.ListProviderModels({provider_id:"google"}) through the REAL connect
// handler in BOTH states, on this host, with the REAL `agy` binary present.
//
// It mirrors internal/server/server.go's injected lister closure exactly (that closure is what production
// wires), so what this prints is what the GUI's useProviderModelsForPicker and the TUI's
// cl.FetchModels receive on the wire.
//
// STATE 1 = agy signed in on this host  -> a non-empty model list
// STATE 2 = agy signed out / absent     -> CodeFailedPrecondition + adapter.AntigravityAuthRequiredMessage
//
// In neither state may it ever be CodeNotFound / "db: not found" — that is criterion 8.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"

	"connectrpc.com/connect"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/adapter"
	"github.com/beardedparrott/orchicon/internal/antigravity"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/providers"
	"github.com/beardedparrott/orchicon/internal/tenant"
)

func main() {
	ctx := tenant.WithID(context.Background(), "tnt_ac8_probe")
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))

	// state1 builds the handler exactly as server.go does: a real agy bridge whose lister closure
	// classifies ErrModelListAuthRequired into providers.ErrAntigravityAuth.
	state1 := func() *providers.Handler {
		h := providers.NewHandler(nil, nil, nil)
		b := antigravity.New(log)
		h.Service().SetAntigravityModelLister(func(c context.Context, tid string) ([]providers.AgyModel, error) {
			rows, err := b.ListModelRows(c, tid)
			if err != nil {
				if errors.Is(err, antigravity.ErrModelListAuthRequired) {
					return nil, providers.ErrAntigravityAuth
				}
				return nil, err
			}
			out := make([]providers.AgyModel, 0, len(rows))
			for _, r := range rows {
				out = append(out, providers.AgyModel{ID: r[0], Name: r[1]})
			}
			return out, nil
		})
		return h
	}

	// state2 forces the signed-out classification the real bridge produces when agy has no credential —
	// the lister returns the sentinel, exactly as server.go's closure does on ErrModelListAuthRequired.
	state2 := func() *providers.Handler {
		h := providers.NewHandler(nil, nil, nil)
		h.Service().SetAntigravityModelLister(func(context.Context, string) ([]providers.AgyModel, error) {
			return nil, providers.ErrAntigravityAuth
		})
		return h
	}

	call := func(label string, h *providers.Handler) map[string]any {
		resp, err := h.ListProviderModels(ctx, connect.NewRequest(&apiv1.ProviderModelsRequest{ProviderId: "google"}))
		out := map[string]any{"state": label}
		if err != nil {
			var ce *connect.Error
			if errors.As(err, &ce) {
				out["grpc_code"] = ce.Code().String()
				out["message"] = ce.Message()
			} else {
				out["grpc_code"] = "unknown"
				out["message"] = err.Error()
			}
			// The two structural claims of AC8, asserted rather than eyeballed.
			out["is_CodeNotFound"] = errors.Is(err, db.ErrNotFound)
			out["is_FailedPrecondition"] = connect.CodeOf(err) == connect.CodeFailedPrecondition
			out["message_is_actionable"] = out["message"] == adapter.AntigravityAuthRequiredMessage
			return out
		}
		ms := resp.Msg.GetModels()
		out["enabled"] = resp.Msg.GetEnabled()
		out["model_count"] = len(ms)
		sample := make([]map[string]any, 0, 3)
		allIDs := make([]string, 0, len(ms))
		for _, m := range ms {
			allIDs = append(allIDs, m.GetId())
		}
		out["all_ids"] = allIDs
		for i, m := range ms {
			if i >= 3 {
				break
			}
			sample = append(sample, map[string]any{
				"id": m.GetId(), "context": m.GetContext(), "source": m.GetSource(),
				"reasoning": m.GetReasoning(), "warn_no_context": m.GetWarnNoContext(),
			})
		}
		out["sample"] = sample
		return out
	}

	res := []map[string]any{
		call("STATE 1: agy signed in (live handshake)", state1()),
		call("STATE 2: agy signed out / absent", state2()),
		call("STATE 3: no lister wired (vendored snapshot)", providers.NewHandler(nil, nil, nil)),
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(res)
}
