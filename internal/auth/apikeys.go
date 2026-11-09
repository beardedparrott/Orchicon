package auth

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"fmt"
)

// APIKeyPrefix is the human-readable prefix on every issued API key.
const APIKeyPrefix = "oc_"

// GenerateApiKey produces a new plaintext API key and its hash.
func GenerateApiKey() (plaintext, prefix, hash string) {
	raw := make([]byte, 32)
	// rand.Read is imported in tokens.go via crypto/rand; reuse it here.
	_, _ = readRand(raw)
	plaintext = APIKeyPrefix + base64.RawURLEncoding.EncodeToString(raw)
	prefix = plaintext[:8] // first 8 chars for identification
	hash = HashApiKey(plaintext)
	return plaintext, prefix, hash
}

// HashApiKey returns the hex SHA-256 hash of the plaintext key.
func HashApiKey(plaintext string) string {
	sum := sha256.Sum256([]byte(plaintext))
	return hex.EncodeToString(sum[:])
}

// VerifyApiKeyHash compares a plaintext key against a stored hash in
// constant time to avoid timing side-channels.
func VerifyApiKeyHash(plaintext, hash string) bool {
	got := HashApiKey(plaintext)
	return subtle.ConstantTimeCompare([]byte(got), []byte(hash)) == 1
}

// ParseBearer extracts the credential from an Authorization header.
func ParseBearer(authzHeader string) (scheme, credential string, err error) {
	if authzHeader == "" {
		return "", "", fmt.Errorf("auth: no authorization header")
	}
	// Split scheme + credential.
	for i := 0; i < len(authzHeader); i++ {
		if authzHeader[i] == ' ' {
			return authzHeader[:i], authzHeader[i+1:], nil
		}
	}
	return "", "", fmt.Errorf("auth: malformed authorization header")
}
