package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
	"golang.org/x/crypto/bcrypt"
)

// Password hashing for the embedded IdP's local accounts. This is the ONLY
// place the plane turns a plaintext password into stored material, and it
// lives at the identity-provider boundary (internal/auth). New hashes are
// argon2id (RFC 9106 parameters, memory-hard) encoded as a self-describing
// PHC string; bcrypt ($2a$/$2b$/$2y$) hashes are accepted on verify so
// existing credentials keep working. No plaintext is ever persisted,
// logged, or returned.

const (
	// MaxPasswordLen bounds password input at the boundary (AGENTS.md:
	// size-bounded inputs to prevent memory-exhaustion abuse). argon2 has
	// no practical length limit, so the cap is purely a resource guard.
	MaxPasswordLen = 1024

	// argon2id parameters (RFC 9106): m=64 MiB, t=3, p=4, keyLen=32.
	// They are carried in the PHC string so a future cost increase is a
	// re-hash, not a schema change.
	argon2Time    = 3
	argon2Memory  = 64 * 1024 // KiB → 64 MiB
	argon2Threads = 4
	argon2KeyLen  = 32
	argon2SaltLen = 16
)

// HashPassword hashes a plaintext password with argon2id and returns the
// self-describing PHC string
// ($argon2id$v=19$m=65536,t=3,p=4$<b64 salt>$<b64 key>). The salt is random
// (crypto/rand), so equal inputs never produce equal hashes.
func HashPassword(password string) (string, error) {
	if len(password) > MaxPasswordLen {
		return "", errors.New("password too long")
	}
	salt := make([]byte, argon2SaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("auth: password salt: %w", err)
	}
	key := argon2.IDKey([]byte(password), salt, argon2Time, argon2Memory, argon2Threads, argon2KeyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argon2Memory, argon2Time, argon2Threads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key)), nil
}

// VerifyPassword checks a plaintext password against a stored self-
// describing hash. It dispatches on the prefix: argon2id re-derives the key
// from the parameters embedded in the PHC string and compares in constant
// time; bcrypt uses bcrypt.CompareHashAndPassword. A malformed or
// unsupported hash returns an error — the caller must treat it as a failed
// login, never a success.
func VerifyPassword(password, stored string) (bool, error) {
	if len(password) > MaxPasswordLen {
		return false, nil
	}
	switch {
	case strings.HasPrefix(stored, "$argon2id$"):
		return verifyArgon2id(password, stored)
	case strings.HasPrefix(stored, "$2a$"),
		strings.HasPrefix(stored, "$2b$"),
		strings.HasPrefix(stored, "$2y$"):
		err := bcrypt.CompareHashAndPassword([]byte(stored), []byte(password))
		if errors.Is(err, bcrypt.ErrMismatchedHashAndPassword) || errors.Is(err, bcrypt.ErrPasswordTooLong) {
			return false, nil
		}
		if err != nil {
			// Any other error is a malformed bcrypt hash, not a mismatch.
			return false, fmt.Errorf("auth: malformed bcrypt hash: %w", err)
		}
		return true, nil
	default:
		return false, fmt.Errorf("auth: unsupported password hash prefix")
	}
}

// verifyArgon2id re-derives the key from the PHC-embedded parameters and
// compares it to the stored key in constant time. The parameter ranges are
// sanity-bounded so a corrupted or stale stored hash cannot force a
// pathological allocation on the login path.
func verifyArgon2id(password, stored string) (bool, error) {
	parts := strings.Split(stored, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false, errors.New("auth: malformed argon2id hash")
	}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil {
		return false, fmt.Errorf("auth: malformed argon2id version: %w", err)
	}
	var memory, iterations uint32
	var threads uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &memory, &iterations, &threads); err != nil {
		return false, fmt.Errorf("auth: malformed argon2id params: %w", err)
	}
	if memory < 8 || memory > 1<<20 { // 8 KiB .. 1 GiB
		return false, errors.New("auth: argon2id memory out of range")
	}
	if iterations == 0 || iterations > 10 {
		return false, errors.New("auth: argon2id iterations out of range")
	}
	if threads == 0 || threads > 16 {
		return false, errors.New("auth: argon2id threads out of range")
	}
	salt, err := b64dec(parts[4])
	if err != nil {
		return false, fmt.Errorf("auth: malformed argon2id salt: %w", err)
	}
	want, err := b64dec(parts[5])
	if err != nil {
		return false, fmt.Errorf("auth: malformed argon2id key: %w", err)
	}
	if len(salt) < 8 || len(want) < 16 || len(want) > 64 {
		return false, errors.New("auth: argon2id hash length out of range")
	}
	got := argon2.IDKey([]byte(password), salt, iterations, memory, threads, uint32(len(want)))
	if subtle.ConstantTimeCompare(got, want) != 1 {
		return false, nil
	}
	return true, nil
}

// b64dec decodes base64 tolerating both padded (StdEncoding) and
// unpadded (RawStdEncoding) encodings.
func b64dec(s string) ([]byte, error) {
	if b, err := base64.RawStdEncoding.DecodeString(s); err == nil {
		return b, nil
	}
	return base64.StdEncoding.DecodeString(s)
}
