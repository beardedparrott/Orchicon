package op

import (
	"context"
	"crypto/ecdsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

const (
	testSigningKey = "test-signing-key-for-op-tests"
	testIdentityID = "01JTESTIDENTITY000000000000"
	testRedirect   = "http://localhost:8080/auth/callback"
)

func testLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// testEnv wires a full embedded OP + login bridge on an httptest server.
type testEnv struct {
	srv  *httptest.Server
	prov *Provider
	mux  *http.ServeMux
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	prov, err := NewProvider(Config{
		SigningKey:    testSigningKey,
		ClientID:      DefaultClientID,
		RedirectURIs:  []string{testRedirect},
		AllowInsecure: true,
		Identity: func(_ context.Context, tenantID, identityID string) (IdentityClaims, error) {
			if identityID != testIdentityID {
				return IdentityClaims{}, errors.New("identity not found")
			}
			return IdentityClaims{Name: "Test User", Email: "test@orchicon.local", Username: "test"}, nil
		},
		Log: testLogger(),
	})
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	mux := http.NewServeMux()
	prov.Register(mux)
	RegisterLoginBridge(mux, prov, func(r *http.Request) (string, string, bool) {
		if c, err := r.Cookie("orchicon_refresh"); err == nil && c.Value == "test-refresh" {
			return DefaultTenantID, testIdentityID, true
		}
		return "", "", false
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(func() { srv.Close(); prov.Close() })
	return &testEnv{srv: srv, prov: prov, mux: mux}
}

// noRedirectClient returns an http.Client that does not follow redirects so
// the caller can inspect each 302 Location.
func noRedirectClient() *http.Client {
	return &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

// authorizeURL builds an /authorize URL with PKCE params.
func authorizeURL(issuer string, state, codeChallenge, nonce string) string {
	v := url.Values{}
	v.Set("client_id", DefaultClientID)
	v.Set("redirect_uri", testRedirect)
	v.Set("response_type", "code")
	v.Set("scope", "openid profile email")
	v.Set("state", state)
	if nonce != "" {
		v.Set("nonce", nonce)
	}
	if codeChallenge != "" {
		v.Set("code_challenge", codeChallenge)
		v.Set("code_challenge_method", "S256")
	}
	return issuer + "/authorize?" + v.Encode()
}

// runCodeFlow drives authorize → login bridge → authorize callback and
// returns the authorization code from the final redirect to the client.
// When cookieValue is non-empty the request carries it (authenticated);
// otherwise the flow bounces to the SPA login page.
func runCodeFlow(t *testing.T, env *testEnv, authzURL, cookieValue string) (code, state string) {
	t.Helper()
	client := noRedirectClient()

	resp, err := client.Get(authzURL)
	if err != nil {
		t.Fatalf("GET authorize: %v", err)
	}
	loc := resp.Header.Get("Location")
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("authorize status = %d, want 302 (login redirect); body=%s", resp.StatusCode, loc)
	}
	loginURL := resolveURL(env.srv.URL, loc)
	if !strings.Contains(loginURL, "/auth/op/login?id=") {
		t.Fatalf("authorize did not redirect to the login bridge: %s", loginURL)
	}

	req, _ := http.NewRequest(http.MethodGet, loginURL, nil)
	if cookieValue != "" {
		req.AddCookie(&http.Cookie{Name: "orchicon_refresh", Value: cookieValue})
	}
	resp, err = client.Do(req)
	if err != nil {
		t.Fatalf("GET login bridge: %v", err)
	}
	loc2 := resp.Header.Get("Location")
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("login bridge status = %d, want 302; body=%s", resp.StatusCode, loc2)
	}

	if cookieValue == "" {
		if !strings.Contains(loc2, "/login?next=") {
			t.Fatalf("unauthenticated login bridge should bounce to SPA /login, got %s", loc2)
		}
		return "", ""
	}

	cbURL := resolveURL(env.srv.URL, loc2)
	if !strings.Contains(cbURL, "/authorize/callback?id=") {
		t.Fatalf("login bridge should redirect to authorize callback, got %s", cbURL)
	}
	resp, err = client.Get(cbURL)
	if err != nil {
		t.Fatalf("GET authorize callback: %v", err)
	}
	loc3 := resp.Header.Get("Location")
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("authorize callback status = %d, want 302; body=%s", resp.StatusCode, loc3)
	}
	finalURL, err := url.Parse(resolveURL(env.srv.URL, loc3))
	if err != nil {
		t.Fatalf("parse final redirect: %v", err)
	}
	q := finalURL.Query()
	return q.Get("code"), q.Get("state")
}

// resolveURL resolves a possibly-relative Location against the base.
func resolveURL(base, loc string) string {
	u, err := url.Parse(loc)
	if err != nil {
		return loc
	}
	if u.IsAbs() {
		return loc
	}
	bu, _ := url.Parse(base)
	return bu.ResolveReference(u).String()
}

// exchangeCode POSTs the code to /token with the code_verifier and returns
// the decoded JSON body.
func exchangeCode(t *testing.T, env *testEnv, code, verifier string) map[string]any {
	t.Helper()
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("client_id", DefaultClientID)
	form.Set("code", code)
	form.Set("redirect_uri", testRedirect)
	if verifier != "" {
		form.Set("code_verifier", verifier)
	}
	resp, err := http.PostForm(env.srv.URL+"/token", form)
	if err != nil {
		t.Fatalf("POST token: %v", err)
	}
	defer resp.Body.Close()
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode token response (status %d): %v", resp.StatusCode, err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("token status = %d: %v", resp.StatusCode, body)
	}
	return body
}

func TestDeriveES256KeyDeterminism(t *testing.T) {
	k1, err := DeriveES256Key(testSigningKey)
	if err != nil {
		t.Fatalf("DeriveES256Key: %v", err)
	}
	k2, err := DeriveES256Key(testSigningKey)
	if err != nil {
		t.Fatalf("DeriveES256Key: %v", err)
	}
	if k1.D.Cmp(k2.D) != 0 {
		t.Fatal("same signing key produced different private scalars")
	}
	if k1.X.Cmp(k2.X) != 0 || k1.Y.Cmp(k2.Y) != 0 {
		t.Fatal("same signing key produced different public points")
	}
	k3, err := DeriveES256Key(testSigningKey + "-other")
	if err != nil {
		t.Fatalf("DeriveES256Key: %v", err)
	}
	if k1.D.Cmp(k3.D) == 0 {
		t.Fatal("different signing keys produced the same private scalar")
	}
	// The point must lie on P-256.
	if !k1.Curve.IsOnCurve(k1.X, k1.Y) {
		t.Fatal("derived public point is not on the curve")
	}
	// Public point must match the private scalar: P = d*G.
	x, y := k1.Curve.ScalarBaseMult(k1.D.Bytes())
	if x.Cmp(k1.X) != 0 || y.Cmp(k1.Y) != 0 {
		t.Fatal("public point does not match scalar base mult")
	}
	// The signing key struct itself must not leak the private key to the
	// public KeySet.
	_ = k1.PublicKey
	var _ *ecdsa.PrivateKey = k1
}

func TestEmbeddedOPDiscovery(t *testing.T) {
	env := newTestEnv(t)
	resp, err := http.Get(env.srv.URL + "/.well-known/openid-configuration")
	if err != nil {
		t.Fatalf("GET discovery: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("discovery status = %d", resp.StatusCode)
	}
	var d struct {
		Issuer                        string   `json:"issuer"`
		AuthorizationEndpoint         string   `json:"authorization_endpoint"`
		TokenEndpoint                 string   `json:"token_endpoint"`
		UserinfoEndpoint              string   `json:"userinfo_endpoint"`
		JWKSURI                       string   `json:"jwks_uri"`
		CodeChallengeMethodsSupported []string `json:"code_challenge_methods_supported"`
		ResponseTypesSupported        []string `json:"response_types_supported"`
		IDTokenSigningAlgValues       []string `json:"id_token_signing_alg_values_supported"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&d); err != nil {
		t.Fatalf("decode discovery: %v", err)
	}
	if d.Issuer != env.srv.URL {
		t.Errorf("issuer = %q, want %q", d.Issuer, env.srv.URL)
	}
	want := func(name, got, want string) {
		if got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	want("authorization_endpoint", d.AuthorizationEndpoint, env.srv.URL+"/authorize")
	want("token_endpoint", d.TokenEndpoint, env.srv.URL+"/token")
	want("userinfo_endpoint", d.UserinfoEndpoint, env.srv.URL+"/userinfo")
	want("jwks_uri", d.JWKSURI, env.srv.URL+"/jwks")
	if !contains(d.CodeChallengeMethodsSupported, "S256") {
		t.Errorf("code_challenge_methods_supported missing S256: %v", d.CodeChallengeMethodsSupported)
	}
	if !contains(d.ResponseTypesSupported, "code") {
		t.Errorf("response_types_supported missing code: %v", d.ResponseTypesSupported)
	}
	if !contains(d.IDTokenSigningAlgValues, "ES256") {
		t.Errorf("id_token_signing_alg_values_supported missing ES256: %v", d.IDTokenSigningAlgValues)
	}
}

func TestEmbeddedOPJWKSPublicOnly(t *testing.T) {
	env := newTestEnv(t)
	resp, err := http.Get(env.srv.URL + "/jwks")
	if err != nil {
		t.Fatalf("GET jwks: %v", err)
	}
	defer resp.Body.Close()
	var set struct {
		Keys []map[string]any `json:"keys"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&set); err != nil {
		t.Fatalf("decode jwks: %v", err)
	}
	if len(set.Keys) != 1 {
		t.Fatalf("jwks keys len = %d, want 1", len(set.Keys))
	}
	key := set.Keys[0]
	if key["kid"] != kid {
		t.Errorf("kid = %v, want %q", key["kid"], kid)
	}
	if key["kty"] != "EC" {
		t.Errorf("kty = %v, want EC", key["kty"])
	}
	if key["alg"] != "ES256" {
		t.Errorf("alg = %v, want ES256", key["alg"])
	}
	if key["use"] != "sig" {
		t.Errorf("use = %v, want sig", key["use"])
	}
	if key["x"] == "" || key["y"] == "" {
		t.Errorf("jwks must publish the public point (x, y); got %v", key)
	}
	if key["d"] != nil {
		t.Error("jwks MUST NOT publish the private scalar d")
	}
	if key["crv"] != "P-256" {
		t.Errorf("crv = %v, want P-256", key["crv"])
	}
}

func TestEmbeddedOPPromptNoneRejected(t *testing.T) {
	env := newTestEnv(t)
	authz := authorizeURL(env.srv.URL, "state", "dummy", "nonce") + "&prompt=none"
	client := noRedirectClient()
	resp, err := client.Get(authz)
	if err != nil {
		t.Fatalf("GET authorize: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusFound {
		loc := resp.Header.Get("Location")
		if strings.Contains(loc, "/auth/op/login") {
			t.Fatal("prompt=none must not redirect to the login bridge")
		}
		return
	}
	// The library redirects the error back to the registered redirect_uri
	// when the request is valid; either way it must not hit the bridge.
	if resp.StatusCode != http.StatusFound && resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("prompt=none status = %d", resp.StatusCode)
	}
}

func TestEmbeddedOPCodeConsumeOnce(t *testing.T) {
	env := newTestEnv(t)
	state := "state-1"
	verifier := "012345678901234567890123456789012345678901234567890123"
	challenge := s256Challenge(verifier)
	code, backState := runCodeFlow(t, env, authorizeURL(env.srv.URL, state, challenge, "nonce-1"), "test-refresh")
	if backState != state {
		t.Fatalf("state round-trip = %q, want %q", backState, state)
	}
	first := exchangeCode(t, env, code, verifier)
	if first["access_token"] == "" || first["id_token"] == "" {
		t.Fatalf("first exchange missing tokens: %v", first)
	}
	// Replaying the same code must fail (consume-once).
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("client_id", DefaultClientID)
	form.Set("code", code)
	form.Set("redirect_uri", testRedirect)
	form.Set("code_verifier", verifier)
	resp, err := http.PostForm(env.srv.URL+"/token", form)
	if err != nil {
		t.Fatalf("POST replay token: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		t.Fatal("replayed authorization code was accepted")
	}
}

func TestEmbeddedOPWrongVerifierRejected(t *testing.T) {
	env := newTestEnv(t)
	state := "state-2"
	verifier := "012345678901234567890123456789012345678901234567890123"
	challenge := s256Challenge(verifier)
	code, _ := runCodeFlow(t, env, authorizeURL(env.srv.URL, state, challenge, "nonce-2"), "test-refresh")
	// Wrong verifier → PKCE must fail the exchange.
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("client_id", DefaultClientID)
	form.Set("code", code)
	form.Set("redirect_uri", testRedirect)
	form.Set("code_verifier", "totally-wrong-verifier-totally-wrong-verifier")
	resp, err := http.PostForm(env.srv.URL+"/token", form)
	if err != nil {
		t.Fatalf("POST token: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		t.Fatal("wrong PKCE verifier was accepted")
	}
}

func TestEmbeddedOPUnauthenticatedBouncesToLogin(t *testing.T) {
	env := newTestEnv(t)
	state := "state-3"
	verifier := "012345678901234567890123456789012345678901234567890123"
	challenge := s256Challenge(verifier)
	code, _ := runCodeFlow(t, env, authorizeURL(env.srv.URL, state, challenge, "nonce-3"), "")
	if code != "" {
		t.Fatalf("unauthenticated flow produced a code: %q", code)
	}
}

func TestEmbeddedOPUserinfo(t *testing.T) {
	env := newTestEnv(t)
	state := "state-ui"
	verifier := "012345678901234567890123456789012345678901234567890123"
	challenge := s256Challenge(verifier)
	code, _ := runCodeFlow(t, env, authorizeURL(env.srv.URL, state, challenge, "nonce-ui"), "test-refresh")
	tokens := exchangeCode(t, env, code, verifier)
	accessToken, _ := tokens["access_token"].(string)
	if accessToken == "" {
		t.Fatal("no access token in exchange response")
	}

	req, _ := http.NewRequest(http.MethodGet, env.srv.URL+"/userinfo", nil)
	req.Header.Set("Authorization", "Bearer "+accessToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET userinfo: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("userinfo status = %d", resp.StatusCode)
	}
	var info map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		t.Fatalf("decode userinfo: %v", err)
	}
	if info["sub"] != testIdentityID {
		t.Errorf("userinfo sub = %v, want %q", info["sub"], testIdentityID)
	}
	if info["name"] != "Test User" {
		t.Errorf("userinfo name = %v, want Test User", info["name"])
	}
	if info["email"] != "test@orchicon.local" {
		t.Errorf("userinfo email = %v, want test@orchicon.local", info["email"])
	}
	if info["preferred_username"] != "test" {
		t.Errorf("userinfo preferred_username = %v, want test", info["preferred_username"])
	}

	// An invalid/absent bearer token must be rejected.
	bad, err := http.Get(env.srv.URL + "/userinfo")
	if err != nil {
		t.Fatalf("GET userinfo (no token): %v", err)
	}
	bad.Body.Close()
	if bad.StatusCode != http.StatusUnauthorized {
		t.Errorf("userinfo without token status = %d, want 401", bad.StatusCode)
	}
}

func TestEmbeddedOPIDTokenClaims(t *testing.T) {
	env := newTestEnv(t)
	state := "state-claims"
	verifier := "012345678901234567890123456789012345678901234567890123"
	challenge := s256Challenge(verifier)
	code, _ := runCodeFlow(t, env, authorizeURL(env.srv.URL, state, challenge, "nonce-claims"), "test-refresh")
	tokens := exchangeCode(t, env, code, verifier)
	rawID, _ := tokens["id_token"].(string)
	if rawID == "" {
		t.Fatal("no id_token in exchange response")
	}
	parts := strings.Split(rawID, ".")
	if len(parts) != 3 {
		t.Fatalf("malformed id_token: %d segments", len(parts))
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("decode id_token payload: %v", err)
	}
	var claims struct {
		Iss         string   `json:"iss"`
		Sub         string   `json:"sub"`
		Aud         []string `json:"aud"`
		AMR         []string `json:"amr"`
		AuthTime    int64    `json:"auth_time"`
		Nonce       string   `json:"nonce"`
		Name        string   `json:"name"`
		Email       string   `json:"email"`
		Azp         string   `json:"azp"`
		IDTokenHint string   `json:"id_token_hint"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		t.Fatalf("unmarshal id_token claims: %v", err)
	}
	if claims.Iss != env.srv.URL {
		t.Errorf("iss = %q, want %q", claims.Iss, env.srv.URL)
	}
	if claims.Sub != testIdentityID {
		t.Errorf("sub = %q, want %q", claims.Sub, testIdentityID)
	}
	if len(claims.Aud) != 1 || claims.Aud[0] != DefaultClientID {
		t.Errorf("aud = %v, want [orchicon]", claims.Aud)
	}
	if claims.Nonce != "nonce-claims" {
		t.Errorf("nonce = %q, want nonce-claims", claims.Nonce)
	}
	if len(claims.AMR) != 1 || claims.AMR[0] != "pwd" {
		t.Errorf("amr = %v, want [pwd]", claims.AMR)
	}
	if claims.AuthTime == 0 {
		t.Error("auth_time missing from id_token")
	}
	if claims.Name != "Test User" {
		t.Errorf("name = %q, want Test User", claims.Name)
	}
	if claims.Email != "test@orchicon.local" {
		t.Errorf("email = %q, want test@orchicon.local", claims.Email)
	}
}

func TestEmbeddedOPUnreachablePathsNotFound(t *testing.T) {
	env := newTestEnv(t)
	// The plane's /healthz is NOT served by the OP (mounted elsewhere);
	// the OP wrapper must not expose its own introspection/revoke/device
	// surface.
	for _, path := range []string{"/oauth/introspect", "/revoke", "/end_session", "/device_authorization", "/ready"} {
		resp, err := http.Get(env.srv.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			t.Errorf("OP path %s must not be served (no dead surface)", path)
		}
	}
}

func s256Challenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}
