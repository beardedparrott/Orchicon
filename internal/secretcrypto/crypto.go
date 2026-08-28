package secretcrypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

// Encrypt encrypts plaintext with AES-256-GCM using kek (must be 32 bytes).
// Returns base64(nonce||ciphertext). Key version is tracked separately in column.
func Encrypt(plaintext []byte, kek []byte) (string, error) {
	if len(kek) != 32 {
		return "", fmt.Errorf("kek must be 32 bytes, got %d", len(kek))
	}
	block, err := aes.NewCipher(kek)
	if err != nil {
		return "", fmt.Errorf("new cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("new gcm: %w", err)
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", fmt.Errorf("rand nonce: %w", err)
	}
	ct := gcm.Seal(nonce, nonce, plaintext, nil)
	return base64.StdEncoding.EncodeToString(ct), nil
}

// Decrypt reverses Encrypt.
func Decrypt(ciphertextB64 string, kek []byte) ([]byte, error) {
	if len(kek) != 32 {
		return nil, fmt.Errorf("kek must be 32 bytes, got %d", len(kek))
	}
	raw, err := base64.StdEncoding.DecodeString(ciphertextB64)
	if err != nil {
		return nil, fmt.Errorf("base64 decode: %w", err)
	}
	block, err := aes.NewCipher(kek)
	if err != nil {
		return nil, fmt.Errorf("new cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("new gcm: %w", err)
	}
	if len(raw) < gcm.NonceSize() {
		return nil, fmt.Errorf("ciphertext too short")
	}
	nonce, ct := raw[:gcm.NonceSize()], raw[gcm.NonceSize():]
	pt, err := gcm.Open(nil, nonce, ct, nil)
	if err != nil {
		return nil, fmt.Errorf("decrypt: %w", err)
	}
	return pt, nil
}

// ParseKEK decodes ORCHICON_SECRETS_KEK (base64 32 bytes or raw 32 chars) for config.
func ParseKEK(s string) ([]byte, error) {
	if s == "" {
		return nil, fmt.Errorf("empty kek")
	}
	// try base64 first
	if b, err := base64.StdEncoding.DecodeString(s); err == nil && len(b) == 32 {
		return b, nil
	}
	if len(s) == 32 {
		return []byte(s), nil
	}
	return nil, fmt.Errorf("kek must be 32 bytes (raw) or base64-encoded 32 bytes")
}

// KEKFileName is the persisted per-instance KEK file, relative to the
// control plane's data dir. It lives next to the instance's other state
// (backups, blob store) so it survives restarts and container rebuilds
// exactly like the instance database — the "KEK persistence == instance
// persistence" invariant. It is never stored in the database itself.
const KEKFileName = "secrets/kek"

// KEKPath returns the absolute path of the persisted KEK file.
func KEKPath(dataDir string) string {
	return filepath.Join(dataDir, "secrets", "kek")
}

// LoadOrCreateKEK returns the instance KEK, generating and persisting a
// fresh 32-byte key on first use. The key file is written atomically
// (temp + rename) with 0600 perms inside a 0700 dir, so a partially
// written key can never be read. An existing file that is not a valid
// 32-byte key is an error (fail-closed — never silently regenerate over
// a key that already encrypts stored secrets).
func LoadOrCreateKEK(dataDir string) ([]byte, error) {
	path := KEKPath(dataDir)
	if data, err := os.ReadFile(path); err == nil {
		if len(data) != 32 {
			return nil, fmt.Errorf("existing KEK file %s is %d bytes, want 32 (do not delete or regenerate it — stored secrets are encrypted with it)", path, len(data))
		}
		return data, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("read KEK file: %w", err)
	}

	kek := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, kek); err != nil {
		return nil, fmt.Errorf("generate KEK: %w", err)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create secrets dir: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".kek-*")
	if err != nil {
		return nil, fmt.Errorf("create KEK temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return nil, fmt.Errorf("chmod KEK temp file: %w", err)
	}
	if _, err := tmp.Write(kek); err != nil {
		tmp.Close()
		return nil, fmt.Errorf("write KEK temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return nil, fmt.Errorf("close KEK temp file: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return nil, fmt.Errorf("persist KEK file: %w", err)
	}
	return kek, nil
}

// ResolveKEK returns the instance KEK for the control plane. An explicit
// ORCHICON_SECRETS_KEK override (operator-managed key — KMS, secret
// manager, shared across replicas) wins; otherwise the per-instance key
// in the data dir is loaded or created on first boot, so the secrets
// store works out of the box with no container/env changes.
func ResolveKEK(override, dataDir string) ([]byte, error) {
	if override != "" {
		return ParseKEK(override)
	}
	return LoadOrCreateKEK(dataDir)
}
