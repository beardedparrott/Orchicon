package secretcrypto

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEncryptDecryptRoundtrip(t *testing.T) {
	kek := make([]byte, 32)
	for i := range kek { kek[i] = byte(i + 1) }
	pt := "TAVILY_API_KEY=tvly-secret-value-123"
	ct, err := Encrypt([]byte(pt), kek)
	if err != nil { t.Fatalf("encrypt: %v", err) }
	if ct == pt { t.Fatal("ciphertext equals plaintext") }
	if strings.Contains(ct, pt) { t.Fatal("ciphertext leaks plaintext") }
	got, err := Decrypt(ct, kek)
	if err != nil { t.Fatalf("decrypt: %v", err) }
	if string(got) != pt { t.Fatalf("roundtrip got %q want %q", string(got), pt) }
	// ciphertext is base64 and varies per nonce
	ct2, _ := Encrypt([]byte(pt), kek)
	if ct == ct2 { t.Log("warning: same nonce (unlikely)") }
	raw, _ := base64.StdEncoding.DecodeString(ct)
	if len(raw) < 12 { t.Fatal("ciphertext too short") }
}

func TestEncryptWrongKEKSize(t *testing.T) {
	if _, err := Encrypt([]byte("x"), make([]byte, 16)); err == nil { t.Fatal("expected error for short kek") }
	if _, err := Decrypt("abc", make([]byte, 16)); err == nil { t.Fatal("expected error for short kek") }
}

func TestDecryptWrongKEK(t *testing.T) {
	k1 := make([]byte, 32); for i := range k1 { k1[i]=1 }
	k2 := make([]byte, 32); for i := range k2 { k2[i]=2 }
	ct, _ := Encrypt([]byte("hello"), k1)
	if _, err := Decrypt(ct, k2); err == nil { t.Fatal("expected decrypt failure with wrong kek") }
}

func TestParseKEK(t *testing.T) {
	raw := "01234567890123456789012345678901"
	k, err := ParseKEK(raw)
	if err != nil { t.Fatalf("raw kek: %v", err) }
	if len(k)!=32 { t.Fatalf("len %d", len(k)) }
	b64 := base64.StdEncoding.EncodeToString([]byte(raw))
	k2, err := ParseKEK(b64)
	if err != nil { t.Fatalf("b64 kek: %v", err) }
	if string(k2)!=raw { t.Fatal("b64 mismatch") }
	if _, err := ParseKEK("short"); err==nil { t.Fatal("expected error for short") }
	if _, err := ParseKEK(""); err==nil { t.Fatal("expected error for empty") }
}

func TestPlaintextNeverPersistedInCiphertext(t *testing.T) {
	kek := make([]byte, 32); for i:=range kek{ kek[i]=byte(42)}
	ct, _ := Encrypt([]byte("super-secret-tavily-key"), kek)
	if strings.Contains(ct, "super-secret") { t.Fatal("plaintext leaked into ciphertext") }
}

func TestLoadOrCreateKEK(t *testing.T) {
	dir := t.TempDir()
	k1, err := LoadOrCreateKEK(dir)
	if err != nil { t.Fatalf("create: %v", err) }
	if len(k1) != 32 { t.Fatalf("len %d", len(k1)) }
	// persisted: a second call returns the SAME key (secrets survive restarts)
	k2, err := LoadOrCreateKEK(dir)
	if err != nil { t.Fatalf("reload: %v", err) }
	if string(k1) != string(k2) { t.Fatal("KEK changed across loads — stored secrets would be lost") }
	// restrictive file perms
	st, err := os.Stat(KEKPath(dir))
	if err != nil { t.Fatalf("stat: %v", err) }
	if st.Mode().Perm() != 0o600 { t.Fatalf("perms %o, want 600", st.Mode().Perm()) }
}

func TestLoadOrCreateKEKRejectsCorruptFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "secrets"), 0o700); err != nil { t.Fatal(err) }
	if err := os.WriteFile(KEKPath(dir), []byte("not-a-32-byte-key"), 0o600); err != nil { t.Fatal(err) }
	if _, err := LoadOrCreateKEK(dir); err == nil {
		t.Fatal("expected error for corrupt KEK file (fail-closed, never regenerate over it)")
	}
}

func TestResolveKEK(t *testing.T) {
	dir := t.TempDir()
	// no override -> per-instance key created on first use
	k1, err := ResolveKEK("", dir)
	if err != nil { t.Fatalf("resolve file: %v", err) }
	if len(k1) != 32 { t.Fatalf("len %d", len(k1)) }
	// explicit override wins
	raw := "01234567890123456789012345678901"
	k2, err := ResolveKEK(raw, dir)
	if err != nil { t.Fatalf("resolve override: %v", err) }
	if string(k2) != raw { t.Fatal("override did not win") }
	// instance key untouched by the override
	k3, err := ResolveKEK("", dir)
	if err != nil { t.Fatalf("resolve file again: %v", err) }
	if string(k1) != string(k3) { t.Fatal("instance KEK changed") }
}
