package auth

import (
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"
)

func TestHashPasswordRoundTrip(t *testing.T) {
	hash, err := HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if !strings.HasPrefix(hash, "$argon2id$v=19$m=65536,t=3,p=4$") {
		t.Fatalf("hash does not carry the expected PHC header: %q", hash)
	}
	ok, err := VerifyPassword("correct horse battery staple", hash)
	if err != nil {
		t.Fatalf("VerifyPassword: %v", err)
	}
	if !ok {
		t.Fatal("VerifyPassword rejected the correct password")
	}
}

func TestHashPasswordUniqueSalt(t *testing.T) {
	h1, err := HashPassword("same password")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	h2, err := HashPassword("same password")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if h1 == h2 {
		t.Fatal("two hashes of the same password are identical — salt not random")
	}
}

func TestVerifyPasswordWrongPassword(t *testing.T) {
	hash, err := HashPassword("correct")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	ok, err := VerifyPassword("wrong", hash)
	if err != nil {
		t.Fatalf("VerifyPassword: %v", err)
	}
	if ok {
		t.Fatal("VerifyPassword accepted the wrong password")
	}
}

func TestVerifyPasswordBcryptCompat(t *testing.T) {
	raw, err := bcrypt.GenerateFromPassword([]byte("bcrypt-password"), bcrypt.DefaultCost)
	if err != nil {
		t.Fatalf("bcrypt.GenerateFromPassword: %v", err)
	}
	stored := string(raw)
	if !strings.HasPrefix(stored, "$2a$") {
		t.Fatalf("unexpected bcrypt prefix: %q", stored)
	}
	ok, err := VerifyPassword("bcrypt-password", stored)
	if err != nil {
		t.Fatalf("VerifyPassword: %v", err)
	}
	if !ok {
		t.Fatal("VerifyPassword rejected a valid bcrypt hash")
	}
	ok, err = VerifyPassword("wrong", stored)
	if err != nil {
		t.Fatalf("VerifyPassword: %v", err)
	}
	if ok {
		t.Fatal("VerifyPassword accepted a wrong password against a bcrypt hash")
	}
}

func TestVerifyPasswordMalformedHash(t *testing.T) {
	cases := []string{
		"",
		"$argon2id$",
		"$argon2id$v=19$m=65536,t=3,p=4$AAAA",
		"$argon2id$v=19$m=65536,t=3,p=4$AAAA$BBBB",
		"$argon2id$v=19$m=0,t=3,p=4$AQIDBAUGBwg$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
		"$scrypt$n=16384,r=8,p=1$AAAA$BBBB",
		"$2a$not-a-valid-bcrypt-hash",
		"plaintext",
	}
	for _, c := range cases {
		ok, err := VerifyPassword("password", c)
		if err == nil {
			t.Errorf("VerifyPassword(%q) = (%v, nil), want error", c, ok)
		}
		if ok {
			t.Errorf("VerifyPassword(%q) = (true, %v), want false", c, err)
		}
	}
}

func TestHashPasswordTooLong(t *testing.T) {
	long := strings.Repeat("a", MaxPasswordLen+1)
	if _, err := HashPassword(long); err == nil {
		t.Fatal("HashPassword accepted an over-long password")
	}
}

func TestVerifyPasswordOverLong(t *testing.T) {
	hash, err := HashPassword("short")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	ok, err := VerifyPassword(strings.Repeat("b", MaxPasswordLen+1), hash)
	if err != nil {
		t.Fatalf("VerifyPassword: %v", err)
	}
	if ok {
		t.Fatal("VerifyPassword accepted an over-long password")
	}
}
