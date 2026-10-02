package aigateway

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"connectrpc.com/connect"
	assets "github.com/beardedparrott/orchicon"
	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/migrate"
	"github.com/beardedparrott/orchicon/internal/tenant"
)

// TestGetUsageCursorPagination pins the GetUsage cursor contract end to
// end against a real Postgres (ORCHICON_TEST_DSN-gated, like the db
// package's cost tests): page_token feeds ListUsageRecordsFilter.AfterID,
// a full page returns next_page_token (the last row's id), a partial page
// returns none, and walking cursor → cursor reproduces every seeded row
// exactly once in (occurred_at, id) descending order. Regression: the RPC
// ignored page_token entirely, so any consumer summing usage_records (the
// Telemetry Credits panel) silently counted only the first page.
func TestGetUsageCursorPagination(t *testing.T) {
	dsn := os.Getenv("ORCHICON_TEST_DSN")
	if dsn == "" {
		t.Skip("ORCHICON_TEST_DSN not set; skipping DB-backed pagination test")
	}
	ctx := context.Background()
	pool, err := db.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open test pool: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := migrate.Run(ctx, pool, assets.MigrationsFS, assets.MigrationsDir); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}

	const testTenant = "tnt_dev"
	const prefix = "usage-pgtest-"
	const session = "usage-pgtest-sess"
	seed := func(t *testing.T) {
		t.Helper()
		ttx, err := pool.BeginTenantTx(ctx, testTenant)
		if err != nil {
			t.Fatalf("begin tenant tx: %v", err)
		}
		defer ttx.Rollback(ctx)
		if _, err := ttx.Tx.Exec(ctx,
			`DELETE FROM usage_records WHERE tenant_id = $1 AND id LIKE $2`, testTenant, prefix+"%"); err != nil {
			t.Fatalf("cleanup seed rows: %v", err)
		}
		base := time.Now().UTC().Add(-time.Hour)
		for i := 1; i <= 7; i++ {
			id := prefix + fmt.Sprintf("%02d", i)
			if _, err := ttx.Tx.Exec(ctx,
				`INSERT INTO usage_records (id, tenant_id, session_id, provider, model, prompt_tokens, completion_tokens, total_tokens, cost_usd, occurred_at, created_at)
				 VALUES ($1, $2, $7, 'test', 'test-model', $3, $3, $4, $5, $6, $6)`,
				id, testTenant, int64(i), int64(2*i), float64(i)*0.5, base.Add(time.Duration(i)*time.Second), session); err != nil {
				t.Fatalf("seed %s: %v", id, err)
			}
		}
		if err := ttx.Commit(ctx); err != nil {
			t.Fatalf("commit seed: %v", err)
		}
	}
	seed(t)
	t.Cleanup(func() { // leave no probe rows behind
		ttx, err := pool.BeginTenantTx(ctx, testTenant)
		if err != nil {
			return
		}
		defer ttx.Rollback(ctx)
		_, _ = ttx.Tx.Exec(ctx, `DELETE FROM usage_records WHERE tenant_id = $1 AND id LIKE $2`, testTenant, prefix+"%")
		_ = ttx.Commit(ctx)
	})

	svc := NewService(pool, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, nil, nil, nil, nil, nil)
	get := func(pageToken string) (*apiv1.GetUsageResponse, error) {
		t.Helper()
		resp, err := svc.GetUsage(tenant.WithID(ctx, testTenant), connect.NewRequest(&apiv1.GetUsageRequest{
			TenantId:  testTenant, // ignored (context-injected) — exercised so the hint stays inert
			SessionId: session,    // scope the walk to the sentinel session: the live tenant table is huge, and cursor+filter is itself the contract
			PageSize:  3,
			PageToken: pageToken,
		}))
		if err != nil {
			return nil, err
		}
		return resp.Msg, nil
	}
	ids := func(r *apiv1.GetUsageResponse) []string {
		t.Helper()
		out := make([]string, 0, len(r.Records))
		for _, rec := range r.Records {
			out = append(out, rec.Id)
		}
		return out
	}
	want := func(t *testing.T, got, expected []string, ctx string) {
		t.Helper()
		if len(got) != len(expected) {
			t.Fatalf("%s: got %v, want %v", ctx, got, expected)
		}
		for i := range expected {
			if got[i] != expected[i] {
				t.Fatalf("%s: got %v, want %v", ctx, got, expected)
			}
		}
	}

	// The tenant may legitimately hold rows from OTHER fixtures/live
	// traffic (the db package's cost tests seed their own and clean at
	// their next run). The contract under test is about MY rows: walk the
	// cursor to exhaustion, assert full pages carry the last-row token,
	// partial pages carry none, an empty page terminates, and the filtered
	// union reproduces the seed exactly once in (occurred_at, id) DESC.
	var walked []string
	tok := ""
	pages := 0
	for {
		page, err := get(tok)
		if err != nil {
			t.Fatalf("page %d (token %q): %v", pages+1, tok, err)
		}
		pages++
		gotIDs := ids(page)
		full := page.NextPageToken != ""
		if full {
			if len(gotIDs) != 3 {
				t.Fatalf("page %d: full page with token must carry pageSize rows, got %d (%v)", pages, len(gotIDs), gotIDs)
			}
			if page.NextPageToken != gotIDs[len(gotIDs)-1] {
				t.Fatalf("page %d: next_page_token %q must equal the last row id %q", pages, page.NextPageToken, gotIDs[len(gotIDs)-1])
			}
		} else if len(gotIDs) == 3 {
			t.Fatalf("page %d: full page returned NO token — the loop would terminate early and undercount", pages)
		}
		// No-repeat keyset contract: a walked id must never reappear.
		seen := map[string]bool{}
		for _, id := range walked {
			seen[id] = true
		}
		for _, id := range gotIDs {
			if seen[id] {
				t.Fatalf("page %d: id %q repeated across adjacent cursors — keyset broken", pages, id)
			}
		}
		walked = append(walked, gotIDs...)
		if !full {
			break
		}
		if tok == page.NextPageToken {
			t.Fatalf("page %d: cursor did not advance (%q) — the walk would loop forever", pages, tok)
		}
		tok = page.NextPageToken
		if pages > 1000 {
			t.Fatalf("cursor walk exceeded 1000 pages — termination broken")
		}
	}

	// The session filter means the walk sees ONLY my 7 seeded rows.
	expect := []string{prefix + "07", prefix + "06", prefix + "05", prefix + "04", prefix + "03", prefix + "02", prefix + "01"}
	want(t, walked, expect, "seed rows across the session-scoped walk")
}
