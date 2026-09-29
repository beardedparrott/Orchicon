package orchicon

// claude_catalog.go — the Claude Code MANAGED model catalog: the authoritative
// model list for the `anthropic` provider, read from the same signed document the
// CLI itself reads.
//
// # Why this exists, and what it replaces
//
// The vendored catalog (internal/orchicon/catalog.json) is a hand-authored
// SNAPSHOT, and it rotted: it listed three `anthropic` models
// (claude-sonnet-4, claude-opus-4, claude-haiku-4) while the CLI's own catalog
// offers twelve on the `cc` surface — and NONE of the three appear in that list
// any more. A snapshot of a moving target is stale the moment it is written; the
// only durable fix is to read the authority.
//
// There is no `claude models` command (that is why the snapshot existed), but the
// CLI maintains a SIGNED REMOTE CATALOG, which is what its own model selector
// reads:
//
//	https://downloads.claude.ai/model-catalog/v1/catalog.json          (~145 KB)
//	https://downloads.claude.ai/model-catalog/v1/catalog.json.raw-sig.json
//
// # The verification, and how it was established
//
// The scheme is not guessed. It was read out of the installed binary (2.1.261,
// which pins the signing key and names the algorithm) and then CONFIRMED
// empirically against a live document:
//
//	signature = RSASSA-PKCS1-v1_5-SHA512( "claude-code-model-catalog-v1\0" || raw )
//
// The `\0`-terminated prefix is domain separation, and it is load-bearing: the
// signature does NOT verify over the raw bytes alone (tried — it fails), so a
// verifier that omitted the prefix would reject every valid catalog.
//
// The key is pinned in the binary and its SPKI SHA-256 is checked against the
// sidecar's own `publicKeySha256`, so a sidecar cannot name a key it was not
// signed with.
//
// # What this catalog does NOT have: pricing — and why that costs us nothing
//
// It carries windows (max_input_tokens / max_output_tokens), effort levels,
// capabilities, family and `offered_on` — and ZERO pricing (verified: no
// price/pricing/cost field occurs anywhere in the document). That is by design
// for a CLIENT catalog, and it would only matter if a catalog had to be the
// pricing source. For claude it does not:
//
//   - THE CLI REPORTS THE COST ITSELF. `total_cost_usd` rides every turn's
//     terminal result (stream.go parses it), internal/claude's mapper records it
//     as UsageInput.CostUSD (parse.go), and UsageRecorder keeps it whenever the
//     catalog declines (recorder.go; see CatalogModelCost's fail-closed contract
//     in catalog_data.go — "the adapter-reported cost stands"). So a model NEWER
//     than the vendored snapshot is still costed, by Claude's own accounting.
//   - the vendored catalog is therefore a REFINEMENT for the models it knows,
//     not the mechanism that makes pricing work. (It remains the only source for
//     an adapter that reports no cost of its own.)
//
// CONSEQUENCE, stated because the comments here previously got it backwards: a
// row left with nil Pricing is a statement about THIS CATALOG, not a claim that
// the model is unpriced. Do not read it as "billing applies".
//
// ONE CAVEAT, and it is Anthropic's own rather than ours: their figure is a USD
// ESTIMATE at list rates, not an invoice — the CLI says exactly that of /cost,
// total_cost_usd, --max-budget-usd and the OTel cost metric alike, and offers a
// `modelPricing` settings block to price at contracted rates instead. For a
// subscription-backed operator it measures PLAN CONSUMPTION at list rates: a
// useful proxy for limit burn, not money spent.

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	// ClaudeCatalogURL is the managed catalog document.
	ClaudeCatalogURL = "https://downloads.claude.ai/model-catalog/v1/catalog.json"
	// claudeCatalogSigSuffix is the detached-signature sidecar's suffix.
	claudeCatalogSigSuffix = ".raw-sig.json"
	// claudeCatalogDomain is the signature's domain-separation prefix. See the
	// file header: without it nothing verifies.
	claudeCatalogDomain = "claude-code-model-catalog-v1\x00"
	// ClaudeCatalogSurface is the surface Orchicon drives. The document carries
	// five (cc, ccd, ccr, chat, cowork) with slightly different sets; `cc` is
	// Claude Code's.
	ClaudeCatalogSurface = "cc"

	// claudeCatalogFetchTimeout bounds the cold fetch. Deliberately short: this
	// runs on the model-picker path, and an unbounded fetch is the shape of the
	// 45-second stall this package was just fixed for. Well inside the picker's
	// 45s read budget, and a failure falls back to the vendored catalog
	// immediately.
	claudeCatalogFetchTimeout = 6 * time.Second
	// claudeCatalogMinTTL floors the cache so a document with a nonsense
	// expires_at cannot cause a fetch per request.
	claudeCatalogMinTTL = 5 * time.Minute
	// claudeCatalogMaxTTL caps it so a long-lived document is still re-read
	// eventually.
	claudeCatalogMaxTTL = 24 * time.Hour
)

// claudeCatalogPublicKeyPEM is the release signing key, PINNED IN THE BINARY and
// extracted from it (2.1.261). Its SPKI SHA-256 is
// 89a24e2e18d2f8fa4627a0a47cf76c3682aef237b60d2cd115d6f5202e2e9896, which is what
// the live sidecar declares — the two agreeing is how the key was confirmed to be
// the right one rather than merely the first present in the binary.
const claudeCatalogPublicKeyPEM = `-----BEGIN PUBLIC KEY-----
MIICIjANBgkqhkiG9w0BAQEFAAOCAg8AMIICCgKCAgEAp28rSV5I8HmK8CK9GixB
UZR/gtJxeOCsRXO4EJiej40jzBmQA3cWXGosVO82ZfFsRKVTtMC5iB/HH9sxjncr
mYNWGroJNbx29m/FgYQBgkCXT4AfFl6rnnXqRGLZOerj/4AqE4yQ1GZbhBgR55Z7
ro0ieKK8RHYUspBKAFHyWRhCCz6THW6YRbf0p/hG/08TOY6Sj3cJ7/AEoTRf9ZmV
NX1k0KvbUSiVGpGY9OIHWgxRJUF2pArU4o/hk+sqGAgEUh8Bjvjwvz6+quLXPg+y
0Y8Ugb1Fg6BUppam/zydYY/Q/+yNjnuF154gD1jEeeir8R5czs6zUHSbo2yXUpAs
IdWYo5End8vGsluVmFExnUWm/fTVMGoM5Wm3v1VRepMydEnJ+atz4oQdmPQcKNAi
p5GJO2uyk++xFr9CpKvlR5jral92toYV/m+mur3va8ydamWBo/qG7/wt0sdS81Iw
H6lcu0SQ39rgKD+bdoPLv05EqVMYTFRI2QZEsWGYTMs0DOrfCIJFH50qyD0x4sWw
1gEWeG3jDgY8cj2StZz+zjqzUd05CibcCzEAGm1EQg5y9D40tIsAU1OI7bpgQ9V0
lC8lrqE7zJY66UK9Z1daA8jrdDi6migNjHFrXfT3V4QvMthCIO05q05SS3x2G3Zp
IgmI+CePUPB1pDf+lhPkU1MCAwEAAQ==
-----END PUBLIC KEY-----`

// claudeCatalogDoc is the document's shape (only what is used).
type claudeCatalogDoc struct {
	Version   int    `json:"version"`
	IssuedAt  string `json:"issued_at"`
	ExpiresAt string `json:"expires_at"`
	Surfaces  map[string]struct {
		ModelSelectorConfig []struct {
			Models []claudeCatalogModel `json:"models"`
		} `json:"model_selector_config"`
	} `json:"surfaces"`
}

type claudeCatalogModel struct {
	ID                   string `json:"id"`
	Name                 string `json:"name"`
	ShortName            string `json:"short_name"`
	MinClaudeCodeVersion string `json:"min_claude_code_version"`
	Runtime              struct {
		MaxInputTokens  int64    `json:"max_input_tokens"`
		MaxOutputTokens int64    `json:"max_output_tokens"`
		EffortLevels    []string `json:"effort_levels"`
		Family          string   `json:"family"`
	} `json:"runtime"`
}

// claudeCatalogSig is the detached signature sidecar.
type claudeCatalogSig struct {
	Algorithm       string `json:"algorithm"`
	Signature       string `json:"signature"`
	PublicKeySha256 string `json:"publicKeySha256"`
}

// ClaudeCatalogKeyFingerprint returns the pinned key's SPKI SHA-256 as lowercase
// hex. Exported so a test can assert the pin against a live sidecar.
func ClaudeCatalogKeyFingerprint() (string, error) {
	blk, _ := pem.Decode([]byte(claudeCatalogPublicKeyPEM))
	if blk == nil {
		return "", fmt.Errorf("claude catalog: the pinned public key is not valid PEM")
	}
	return hex.EncodeToString(sha256Sum(blk.Bytes)), nil
}

func sha256Sum(b []byte) []byte {
	d := sha256.Sum256(b)
	return d[:]
}

// VerifyClaudeCatalog checks a catalog document against its detached signature.
//
// `wantKeySha256` (when non-empty) must equal the fingerprint of the key we pin,
// so a sidecar signed under a DIFFERENT key is refused even if that key is
// otherwise valid. The signature is then verified over
// (domain-prefix || raw) — see the file header; the raw bytes alone do NOT
// verify.
func VerifyClaudeCatalog(raw []byte, sig claudeCatalogSig, wantKeySha256 string) error {
	return verifyClaudeCatalogWith(raw, sig, wantKeySha256, true)
}

// verifyClaudeCatalogWith is VerifyClaudeCatalog with the domain prefix made
// optional, so a test can DEMONSTRATE that the prefix is load-bearing (without
// it, a valid document does not verify) rather than merely asserting it in a
// comment. Production always passes true.
func verifyClaudeCatalogWith(raw []byte, sig claudeCatalogSig, wantKeySha256 string, withDomainPrefix bool) error {
	if !strings.Contains(strings.ToUpper(sig.Algorithm), "PKCS1") {
		return fmt.Errorf("claude catalog: unsupported signature algorithm %q", sig.Algorithm)
	}
	blk, _ := pem.Decode([]byte(claudeCatalogPublicKeyPEM))
	if blk == nil {
		return fmt.Errorf("claude catalog: the pinned public key is not valid PEM")
	}
	pubAny, err := x509.ParsePKIXPublicKey(blk.Bytes)
	if err != nil {
		return fmt.Errorf("claude catalog: parse the pinned key: %w", err)
	}
	pub, ok := pubAny.(*rsa.PublicKey)
	if !ok {
		return fmt.Errorf("claude catalog: the pinned key is not RSA")
	}
	got := hex.EncodeToString(sha256Sum(blk.Bytes))
	if wantKeySha256 != "" && !strings.EqualFold(strings.TrimSpace(wantKeySha256), got) {
		return fmt.Errorf("claude catalog: the sidecar names a different signing key (%s) than the pinned one (%s)",
			strings.TrimSpace(wantKeySha256), got)
	}

	sigBytes, err := base64.StdEncoding.DecodeString(strings.TrimSpace(sig.Signature))
	if err != nil {
		return fmt.Errorf("claude catalog: the signature is not valid base64: %w", err)
	}

	msg := make([]byte, 0, len(claudeCatalogDomain)+len(raw))
	if withDomainPrefix {
		msg = append(msg, claudeCatalogDomain...)
	}
	msg = append(msg, raw...)
	digest := sha512.Sum512(msg)

	if err := rsa.VerifyPKCS1v15(pub, crypto.SHA512, digest[:], sigBytes); err != nil {
		return fmt.Errorf("claude catalog: signature verification failed: %w", err)
	}
	return nil
}

// ParseClaudeCatalog extracts one surface's models as ModelInfo rows.
//
// `tools` is set true: every model this catalog offers is a Claude Code model,
// and Claude Code is an agentic tool-user by definition. Reasoning follows the
// effort levels — a model with none is not a reasoning model, which is the
// document's own signal rather than a guess.
//
// IDS ARE BARE (no `anthropic/` prefix), matching ModelInfo.ID's contract; the
// provider is applied by the caller.
func ParseClaudeCatalog(raw []byte, surface string) ([]ModelInfo, error) {
	var doc claudeCatalogDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("claude catalog: parse: %w", err)
	}
	s, ok := doc.Surfaces[surface]
	if !ok {
		return nil, fmt.Errorf("claude catalog: no %q surface in the document", surface)
	}
	tools := true
	seen := map[string]bool{}
	var out []ModelInfo
	for _, cfg := range s.ModelSelectorConfig {
		for _, m := range cfg.Models {
			id := strings.TrimSpace(m.ID)
			if id == "" || seen[id] {
				continue
			}
			seen[id] = true
			mi := ModelInfo{
				ID:        id,
				Context:   m.Runtime.MaxInputTokens,
				MaxOutput: m.Runtime.MaxOutputTokens,
				Tools:     &tools,
				Visible:   true,
			}
			if len(m.Runtime.EffortLevels) > 0 {
				mi.ReasoningEfforts = append([]string(nil), m.Runtime.EffortLevels...)
			}
			out = append(out, mi)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// --- fetch, verify, cache ----------------------------------------------------

type claudeCatalogCache struct {
	mu      sync.Mutex
	models  []ModelInfo
	fetched time.Time
	expires time.Time
	lastErr error
}

var claudeCatalogState claudeCatalogCache

// setClaudeCatalogClientForTest swaps the HTTP client (tests only).
var claudeCatalogClient = &http.Client{Timeout: claudeCatalogFetchTimeout}

// ClaudeCatalogModels returns the managed catalog's models for the `anthropic`
// provider, with pricing merged from the vendored catalog.
//
// Cached until the document's own `expires_at` (bounded). Returns nil when the
// catalog cannot be fetched or VERIFIED — a failed signature is treated exactly
// like a network failure, because an unverified document is not a source — and
// the caller falls back to the vendored list rather than showing nothing.
func ClaudeCatalogModels(ctx context.Context) []ModelInfo {
	if cached, ok := claudeCatalogFromCache(); ok {
		return cached
	}
	models, err := fetchAndVerifyClaudeCatalog(ctx)
	if err != nil {
		claudeCatalogState.mu.Lock()
		claudeCatalogState.lastErr = err
		claudeCatalogState.mu.Unlock()
		return nil
	}
	return mergeClaudePricing(models)
}

// ClaudeCatalogLastError reports the most recent fetch/verify failure (diagnostics
// / the honest-degradation log). Nil when the last attempt succeeded.
func ClaudeCatalogLastError() error {
	claudeCatalogState.mu.Lock()
	defer claudeCatalogState.mu.Unlock()
	return claudeCatalogState.lastErr
}

func claudeCatalogFromCache() ([]ModelInfo, bool) {
	claudeCatalogState.mu.Lock()
	defer claudeCatalogState.mu.Unlock()
	if claudeCatalogState.models == nil || time.Now().After(claudeCatalogState.expires) {
		return nil, false
	}
	return claudeCatalogState.models, true
}

// storeClaudeCatalog caches a verified document until its own expiry.
func storeClaudeCatalog(models []ModelInfo, docExpiry time.Time) {
	ttl := time.Until(docExpiry)
	if ttl < claudeCatalogMinTTL {
		ttl = claudeCatalogMinTTL
	}
	if ttl > claudeCatalogMaxTTL {
		ttl = claudeCatalogMaxTTL
	}
	claudeCatalogState.mu.Lock()
	claudeCatalogState.models = models
	claudeCatalogState.fetched = time.Now()
	claudeCatalogState.expires = time.Now().Add(ttl)
	claudeCatalogState.lastErr = nil
	claudeCatalogState.mu.Unlock()
}

func fetchAndVerifyClaudeCatalog(ctx context.Context) ([]ModelInfo, error) {
	ctx, cancel := context.WithTimeout(ctx, claudeCatalogFetchTimeout)
	defer cancel()

	raw, err := claudeCatalogGet(ctx, ClaudeCatalogURL)
	if err != nil {
		return nil, fmt.Errorf("claude catalog: fetch: %w", err)
	}
	sigRaw, err := claudeCatalogGet(ctx, ClaudeCatalogURL+claudeCatalogSigSuffix)
	if err != nil {
		return nil, fmt.Errorf("claude catalog: fetch signature: %w", err)
	}
	var sig claudeCatalogSig
	if err := json.Unmarshal(sigRaw, &sig); err != nil {
		return nil, fmt.Errorf("claude catalog: parse signature: %w", err)
	}
	if err := VerifyClaudeCatalog(raw, sig, sig.PublicKeySha256); err != nil {
		return nil, err
	}
	models, err := ParseClaudeCatalog(raw, ClaudeCatalogSurface)
	if err != nil {
		return nil, err
	}
	if len(models) == 0 {
		return nil, fmt.Errorf("claude catalog: verified but empty for surface %q", ClaudeCatalogSurface)
	}

	// The document's own expiry drives the cache TTL; an unparseable one just
	// means the bounded default.
	var expiry time.Time
	var doc claudeCatalogDoc
	if json.Unmarshal(raw, &doc) == nil && doc.ExpiresAt != "" {
		if t, perr := time.Parse(time.RFC3339, doc.ExpiresAt); perr == nil {
			expiry = t
		}
	}
	storeClaudeCatalog(models, expiry)
	return models, nil
}

func claudeCatalogGet(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := claudeCatalogClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: http %d", url, resp.StatusCode)
	}
	// Bounded read: the document is ~150 KB; anything vastly larger is not it.
	return io.ReadAll(io.LimitReader(resp.Body, 8<<20))
}

// mergeClaudePricing copies vendored pricing onto catalog rows by id.
//
// A model with no vendored entry keeps a nil Pricing, and that is a statement
// about THIS CATALOG rather than about the model being unpriced: the recorded
// cost for a claude turn comes from the CLI's own `total_cost_usd` whenever the
// catalog declines (see the file header), so a row newer than the snapshot is
// still costed at runtime. This function only decides which authored rates the
// picker prefers where we have them.
func mergeClaudePricing(models []ModelInfo) []ModelInfo {
	byID := map[string]*Pricing{}
	for _, m := range CatalogModelsForProvider("anthropic") {
		if m.Pricing != nil {
			byID[m.ID] = m.Pricing
		}
	}
	out := make([]ModelInfo, len(models))
	copy(out, models)
	for i := range out {
		if p, ok := byID[out[i].ID]; ok {
			out[i].Pricing = p
		}
	}
	return out
}
