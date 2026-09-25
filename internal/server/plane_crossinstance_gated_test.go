package server

// The instance-binding of a plane credential — the ONLY thing standing
// between a misconfigured ORCHICON_PLANE_URL and an automation worker writing
// into the WRONG instance's database.
//
// A plane credential is minted into (and resolved from) the minting
// instance's OWN database: database.A holds the key's hash for a run on A. If
// that run's container is pointed at instance B's plane — a stale
// ORCHICON_PLANE_PUBLIC_URL exported from a shared profile, a launcher that
// hands one instance's URL to the other's workers — B holds no matching hash,
// so B REJECTS the call (401). The result is a loud, broken run; not a silent
// cross-instance write.
//
// That is a property nobody can see in the code, and it disappears the moment
// a future change shares a database between instances (then B WOULD resolve
// the key and accept the write). Hence the explicit test.
//
// Gated on the DB-backed pattern this package already uses
// (adapter_kind_test.go): ORCHICON_TEST_DSN for THIS instance's database plus
// ORCHICON_TEST_PLANE_PROD_URL for the OTHER instance's plane base URL. A
// missing prerequisite is a loud SKIP, never a silent pass.
//
// Optional: set ORCHICON_TEST_PLANE_URL (this instance's own plane base URL)
// to also assert the positive direction on the wire — the same key IS
// accepted by the plane that minted it.

import (
	"context"

	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1/apiv1connect"
	"github.com/beardedparrott/orchicon/internal/auth"
	"github.com/beardedparrott/orchicon/internal/db"
)

// bearerTransport adds the Authorization header the plane's auth middleware
// parses (the same header internal/mcp's plane registry sets).
type bearerTransport struct {
	base  http.RoundTripper
	token string
}

func (t *bearerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	clone.Header.Set("Authorization", "Bearer "+t.token)
	return t.base.RoundTrip(clone)
}

// dialListWorkItems presents token to planeURL's Connect API. It returns the
// connect error code (0 for success) so the assertions read as
// "rejected/accepted", not as string matching.
func dialListWorkItems(t *testing.T, planeURL, token string) (connect.Code, error) {
	t.Helper()
	hc := &http.Client{
		Transport: &bearerTransport{base: http.DefaultTransport, token: token},
		Timeout:   20 * time.Second,
	}
	cli := apiv1connect.NewWorkItemServiceClient(hc, planeURL)
	_, err := cli.ListWorkItems(context.Background(), connect.NewRequest(&apiv1.ListWorkItemsRequest{}))
	if err == nil {
		return 0, nil
	}
	return connect.CodeOf(err), err
}

func TestCrossInstancePlaneCredentialIsRejected(t *testing.T) {
	dsn := os.Getenv("ORCHICON_TEST_DSN")
	otherURL := strings.TrimRight(os.Getenv("ORCHICON_TEST_PLANE_PROD_URL"), "/")
	if dsn == "" || otherURL == "" {
		t.Skip("cross-instance plane-credential test needs ORCHICON_TEST_DSN (this instance's DB) " +
			"AND ORCHICON_TEST_PLANE_PROD_URL (the OTHER instance's plane base URL); skipping")
	}

	ctx := context.Background()
	pool, err := db.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open test pool: %v", err)
	}
	t.Cleanup(pool.Close)

	tenant := "tnt_dev"
	uniq := db.NewID()[:10]

	// Mint a live, active credential in THIS instance's database only.
	ttx, err := pool.BeginTenantTx(ctx, tenant)
	if err != nil {
		t.Fatalf("begin tenant tx: %v", err)
	}
	ident, err := db.CreateIdentity(ctx, ttx.Tx, db.IdentityRow{
		TenantID:     tenant,
		Subject:      "xinstance-guard-" + uniq,
		DisplayName:  "Cross-instance guard",
		IdentityType: "user",
	})
	if err != nil {
		_ = ttx.Rollback(ctx)
		t.Fatalf("create identity: %v", err)
	}
	plaintext, prefix, hash := auth.GenerateApiKey()
	if _, err := db.CreateApiKey(ctx, ttx.Tx, db.ApiKeyRow{
		TenantID:   tenant,
		IdentityID: ident.ID,
		Name:       "xinstance-guard-key",
		KeyPrefix:  prefix,
		KeyHash:    hash,
		Scopes:     []string{"work_item:read"},
	}); err != nil {
		_ = ttx.Rollback(ctx)
		t.Fatalf("create api key: %v", err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	// The identity delete cascades to its api_keys rows.
	t.Cleanup(func() {
		cttx, err := pool.BeginTenantTx(context.Background(), tenant)
		if err != nil {
			return
		}
		_, _ = cttx.Tx.Exec(context.Background(), "DELETE FROM identities WHERE id = $1", ident.ID)
		_ = cttx.Commit(context.Background())
	})

	// Non-vacuous setup: the credential IS resolvable HERE. Without this the
	// rejection below could just mean "the key was never valid anywhere".
	row, err := db.LookupApiKeyByHash(ctx, pool, hash)
	if err != nil || row.ID == "" {
		t.Fatalf("the minted credential must resolve in the minting instance's DB (row=%+v err=%v)", row, err)
	}
	if row.TenantID != tenant {
		t.Fatalf("resolved key tenant = %q, want %q", row.TenantID, tenant)
	}

	// THE ASSERTION: the other instance's plane holds no matching hash, so it
	// rejects the credential instead of honouring it. Rejected — not a
	// success-shaped response.
	code, err := dialListWorkItems(t, otherURL, plaintext)
	switch code {
	case connect.CodeUnauthenticated, connect.CodePermissionDenied:
		t.Logf("misaddressed dial rejected by %s as %v: %v", otherURL, code, err)
	default:
		t.Fatalf("a credential minted for THIS instance was NOT rejected by the other plane %s "+
			"(code=%v err=%v). The instance-binding of the plane credential is the only guard against a "+
			"misconfigured ORCHICON_PLANE_URL writing into the wrong instance's database — if the two "+
			"instances now share a database, this guard is gone and must be replaced deliberately.", otherURL, code, err)
	}

	// Positive direction (optional): the plane that minted it does accept it,
	// which is what makes the rejection above a statement about instance
	// binding rather than about the key being dead.
	if ownURL := strings.TrimRight(os.Getenv("ORCHICON_TEST_PLANE_URL"), "/"); ownURL != "" {
		ownCode, ownErr := dialListWorkItems(t, ownURL, plaintext)
		if ownCode == connect.CodeUnauthenticated || ownCode == connect.CodePermissionDenied {
			t.Fatalf("the minting instance's own plane %s rejected its credential (code=%v err=%v) — "+
				"the setup credential is not actually valid, so the cross-instance assertion proves nothing", ownURL, ownCode, ownErr)
		}
		t.Logf("the minting plane %s accepted the credential (code=%v)", ownURL, ownCode)
	} else {
		t.Logf("ORCHICON_TEST_PLANE_URL unset: positive direction asserted only against the DB (key resolves) — " +
			"set it to also prove the wire acceptance on the minting plane")
	}
}
