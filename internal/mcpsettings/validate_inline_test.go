package mcpsettings_test

// validate_inline_test.go — the INLINE half of definition validation, which had no gate at all before
// it existed: a worker version's `permissions.mcp_servers` is opaque JSON to the worker service, so a
// spec could name an env key that no environment can carry (the stdio transport hands those to a child
// process as k=v) and could reference a ${SECRET_NAME} that does not exist (which then surfaced only
// when the worker RAN, per server, in ResolveSecretRefs).
//
// The shape rules are asserted WITHOUT a database — the tx parameter is only ever used by the
// secret-existence check, and a spec with no ${REF} never reaches it — so these run everywhere. The
// reference rule is asserted against a real store, like the rest of this suite.
//
//	export ORCHICON_TEST_DSN='postgres://orchicon:orchicon@localhost:5432/orchicon_test?sslmode=disable'
//	go test ./internal/mcpsettings/ -run InlineSpec -v

import (
	"context"
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/mcpsettings"
)

// permissionsWith wraps a spec list in the version permissions shape the field carries.
func permissionsWith(specs string) []byte {
	return []byte(`{"tools":["read","write"],"mcp_servers":[` + specs + `]}`)
}

// THE RULES AN OWNED ROW ALREADY HAS APPLY TO AN INLINE SPEC TOO.
//
// Each case is one rule, each names the spec it is about (an operator editing a version needs to know
// WHICH server was refused), and each is the same message the owned-row path gives for the same fault.
func TestInlineSpecsRefuseWhatAnOwnedRowRefuses(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		spec string
		want string
	}{
		{
			name: "env key no environment can carry",
			spec: `{"id":"gh","type":"stdio","command":["npx","-y","gh"],"env":{"NOT A KEY":"x"}}`,
			want: `inline server "gh": env key "NOT A KEY"`,
		},
		{
			name: "header key with an equals sign",
			spec: `{"id":"remote","type":"http","url":"https://mcp.example/sse","headers":{"A=B":"x"}}`,
			want: `inline server "remote": headers key "A=B"`,
		},
		{
			name: "stdio with no command",
			spec: `{"id":"gh","type":"stdio","env":{"TOKEN":"x"}}`,
			want: `inline server "gh": a stdio server needs a command`,
		},
		{
			name: "http with a relative url",
			spec: `{"id":"remote","type":"http","url":"/mcp"}`,
			want: `inline server "remote": url must be an absolute http(s) URL`,
		},
		{
			name: "command over the bound",
			spec: `{"id":"gh","type":"stdio","command":["` + strings.Repeat("x", 1025) + `"]}`,
			want: `inline server "gh": command too long`,
		},
		{
			name: "argument over the bound",
			spec: `{"id":"gh","type":"stdio","command":["npx","` + strings.Repeat("y", 257) + `"]}`,
			want: `inline server "gh": arg too long`,
		},
		{
			name: "id over the bound",
			spec: `{"id":"` + strings.Repeat("z", 201) + `","type":"stdio","command":["npx"]}`,
			want: `inline server id too long`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// nil tx: these faults are all decided before any store lookup (see the file comment).
			err := mcpsettings.ValidateInlinePermissions(ctx, nil, "tnt_unused", nil, permissionsWith(tc.spec))
			if err == nil {
				t.Fatalf("the spec was accepted: %s", tc.spec)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("the refusal does not name the fault:\n got: %v\nwant: %s", err, tc.want)
			}
		})
	}
}

// THE SPECS THAT ARE FINE STAY FINE — both transports, and the two shapes the resolver reads.
func TestInlineSpecsAcceptWhatResolves(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		body string
	}{
		{"stdio with argv and env", `{"mcp_servers":[{"id":"gh","type":"stdio","command":["npx","-y","server-github"],"env":{"A":"b","_C9":"d"}}]}`},
		{"http with a url and headers", `{"mcp_servers":[{"id":"remote","type":"http","url":"https://mcp.example/sse","headers":{"Authorization":"Bearer x"}}]}`},
		{"legacy single-string command", `{"mcp_servers":[{"id":"old","command":"npx -y server-old"}]}`},
		{"disabled spec", `{"mcp_servers":[{"id":"gh","type":"stdio","command":["npx"],"enabled":false}]}`},
		{"no mcp_servers key at all", `{"tools":["read"]}`},
		{"empty permissions", ``},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := mcpsettings.ValidateInlinePermissions(ctx, nil, "tnt_unused", nil, []byte(tc.body)); err != nil {
				t.Errorf("a spec that resolves was refused: %v", err)
			}
		})
	}
}

// ONLY WHAT THE EDIT CHANGES IS JUDGED, and this is the contract that keeps stored data working: every
// client sends the permissions blob WHOLE, so validating the incoming set in full would refuse ANY edit
// of a version that happens to carry a stale spec (a reference to a secret deleted since, a key typed
// before this gate existed). A spec identical to the stored one is left alone; the moment it CHANGES,
// the rules apply.
func TestInlineSpecValidationJudgesOnlyTheChange(t *testing.T) {
	ctx := context.Background()
	stale := permissionsWith(`{"id":"gh","type":"stdio","command":["npx","-y","gh"],"env":{"NOT A KEY":"x"}}`)

	// UNCHANGED — including a pure reformat, because the comparison is on the PARSED spec: the stored
	// blob is pretty-printed and the incoming one is compact, and they are the same spec.
	reformatted := []byte(`{"mcp_servers":[{ "id": "gh", "type": "stdio", "command": ["npx", "-y", "gh"], "env": {"NOT A KEY": "x"} }]}`)
	if err := mcpsettings.ValidateInlinePermissions(ctx, nil, "tnt_unused", stale, reformatted); err != nil {
		t.Errorf("an edit that did not touch the stored spec re-judged it: %v", err)
	}

	// CHANGED — the same spec with a key fixed to a legal one and one left broken → the broken one is
	// judged (and refused) because the spec it belongs to is no longer identical to the stored one.
	edited := permissionsWith(`{"id":"gh","type":"stdio","command":["npx","-y","gh"],"env":{"NOT A KEY":"x","GOOD":"y"}}`)
	if err := mcpsettings.ValidateInlinePermissions(ctx, nil, "tnt_unused", stale, edited); err == nil {
		t.Error("a CHANGED spec carrying a key no environment can carry was accepted — the stored copy " +
			"being broken does not license a new one")
	}

	// A NEW spec is judged with no stored counterpart at all.
	if err := mcpsettings.ValidateInlinePermissions(ctx, nil, "tnt_unused", nil,
		permissionsWith(`{"id":"new","type":"stdio","env":{"TOKEN":"x"}}`)); err == nil {
		t.Error("a brand new spec with no command was accepted")
	}
}

// EVERY ${SECRET_NAME} AN INLINE SPEC CARRIES MUST EXIST, which is the rule that used to be discovered
// only when the worker ran (ResolveSecretRefs reports an unset reference per server, at session time).
// This one needs the store, so it is DB-backed like the rest of this suite.
func TestInlineSpecSecretRefsMustExist(t *testing.T) {
	svc, pool := newTestService(t)
	_ = svc
	ctx := context.Background()
	cleanupMCP(t, pool)

	// Store one credential the way the store's own writer does (any ciphertext — this rule asks whether
	// a secret EXISTS, and nothing here decrypts it).
	ttx, err := pool.BeginTenantTx(ctx, testTenant)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer ttx.Rollback(ctx)
	if _, err := db.CreateSecret(ctx, ttx.Tx, db.SecretRow{
		ID: db.NewID(), TenantID: testTenant, Name: "MCP_GITHUB_GITHUB_PERSONAL_ACCESS_TOKEN",
		Description: "seeded for the inline-ref test", Ciphertext: "not-a-real-ciphertext", KeyVersion: 1,
	}); err != nil {
		t.Fatalf("create secret: %v", err)
	}

	// A reference to the stored secret is fine.
	ok := permissionsWith(`{"id":"gh","type":"stdio","command":["npx","-y","gh"],` +
		`"env":{"GITHUB_PERSONAL_ACCESS_TOKEN":"${MCP_GITHUB_GITHUB_PERSONAL_ACCESS_TOKEN}"}}`)
	if err := mcpsettings.ValidateInlinePermissions(ctx, ttx.Tx, testTenant, nil, ok); err != nil {
		t.Errorf("a reference to a STORED secret was refused: %v", err)
	}

	// A reference to a secret nobody stored is refused, and the message names the secret and the server.
	missing := permissionsWith(`{"id":"gh","type":"stdio","command":["npx","-y","gh"],` +
		`"env":{"GITHUB_PERSONAL_ACCESS_TOKEN":"${NEVER_STORED}"}}`)
	err = mcpsettings.ValidateInlinePermissions(ctx, ttx.Tx, testTenant, nil, missing)
	if err == nil {
		t.Fatal("a reference to a secret that is not stored was accepted — the version would save and " +
			"then fail every session that resolved it")
	}
	for _, want := range []string{"NEVER_STORED", `inline server "gh"`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not name %q: %v", want, err)
		}
	}

	// The same rule applies to HEADERS on an http spec.
	missingHeader := permissionsWith(`{"id":"remote","type":"http","url":"https://mcp.example/sse",` +
		`"headers":{"Authorization":"${NEVER_STORED}"}}`)
	if err := mcpsettings.ValidateInlinePermissions(ctx, ttx.Tx, testTenant, nil, missingHeader); err == nil {
		t.Error("an http spec's header reference to a missing secret was accepted")
	}
}
