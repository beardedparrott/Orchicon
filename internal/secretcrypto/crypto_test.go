package secretcrypto

import (
	"encoding/base64"
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
