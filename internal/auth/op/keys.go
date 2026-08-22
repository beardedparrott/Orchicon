// Package op hosts the embedded OpenID Provider (zitadel/oidc v3, pkg/op)
// mounted on the control-plane origin. It is the server-side face of
// Orchicon's auth: a first-party RP can run a real authorization-code +
// PKCE flow against the plane itself, while the existing BYO-IdP relying
// party (internal/auth/oidc.go) stays byte-for-byte unchanged.
//
// This package is the ONLY place in the tree that imports zitadel/oidc;
// the rest of internal/auth is wired to it through internal/auth/op_wire.go.
package op

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hkdf"
	"crypto/sha256"
	"errors"
	"math/big"

	jose "github.com/go-jose/go-jose/v4"
)

// kid is the stable key id for the OP's signing key. The derived key is
// deterministic, so the id never changes across restarts and short-lived
// previously-issued ID tokens stay verifiable after a reboot.
const kid = "orchicon-op-v1"

// DeriveES256Key deterministically derives an ES256 (ECDSA P-256) keypair
// from the auth signing key:
//
//	seed = HKDF-SHA256(ikm = SigningKey, salt = nil, info = "orchicon:op:ecdsa-p256")
//	d    = seed reduced mod the curve order N (d==0 → 1)
//	(x,y)= d*G
//
// No crypto/rand is involved, so the same signing key always yields the
// same keypair. The OP signs ID tokens with ES256 (NOT the literal HS256
// HMAC key): the acceptance criterion forces the OP's public JWKS to
// publish the verification key, and for a symmetric key that key IS the
// access-token signing secret — anyone who fetched /jwks could forge
// Orchicon access/refresh tokens. The derived public point is all that
// ever leaves the process.
func DeriveES256Key(signingKey string) (*ecdsa.PrivateKey, error) {
	if signingKey == "" {
		return nil, errors.New("auth/op: empty signing key")
	}
	curve := elliptic.P256()
	d := hkdfScalar([]byte(signingKey), "orchicon:op:ecdsa-p256", curve.Params().N)
	x, y := curve.ScalarBaseMult(d.Bytes())
	return &ecdsa.PrivateKey{
		PublicKey: ecdsa.PublicKey{Curve: curve, X: x, Y: y},
		D:         d,
	}, nil
}

// DeriveCryptoKey deterministically derives the [32]byte key the OP uses
// to encrypt opaque bearer access tokens (info "orchicon:op:enc"). Same
// key material family as the signing key, separate usage domain.
func DeriveCryptoKey(signingKey string) [32]byte {
	var out [32]byte
	seed, err := hkdf.Key(sha256.New, []byte(signingKey), nil, "orchicon:op:enc", 32)
	if err != nil {
		return out
	}
	copy(out[:], seed)
	return out
}

// hkdfScalar expands the signing key with HKDF-SHA256 into a 32-byte
// scalar, reduced mod the given curve order (or left raw when mod is nil),
// and guards against a degenerate zero scalar.
func hkdfScalar(ikm []byte, info string, mod *big.Int) *big.Int {
	buf, err := hkdf.Key(sha256.New, ikm, nil, info, 32)
	if err != nil {
		// A pure in-memory HKDF expansion cannot fail with a nil hash;
		// fall back to a raw copy so callers get a stable key.
		buf = make([]byte, 32)
		copy(buf, ikm)
	}
	d := new(big.Int).SetBytes(buf)
	if mod != nil {
		d.Mod(d, mod)
	}
	if d.Sign() == 0 {
		d.SetInt64(1)
	}
	return d
}

// signingKey implements op.SigningKey for the derived ES256 private key.
type signingKey struct {
	key *ecdsa.PrivateKey
}

func (k *signingKey) SignatureAlgorithm() jose.SignatureAlgorithm { return jose.ES256 }
func (k *signingKey) Key() any                                    { return k.key }
func (k *signingKey) ID() string                                  { return kid }

// publicKey implements op.Key for the JWKS endpoint. It carries ONLY the
// public ECDSA point — the private scalar never leaves the process.
type publicKey struct {
	pub *ecdsa.PublicKey
}

func (k *publicKey) ID() string                         { return kid }
func (k *publicKey) Algorithm() jose.SignatureAlgorithm { return jose.ES256 }
func (k *publicKey) Use() string                        { return "sig" }
func (k *publicKey) Key() any                           { return k.pub }
