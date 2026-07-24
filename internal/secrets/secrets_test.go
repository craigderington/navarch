package secrets

import (
	"os"
	"path/filepath"
	"testing"
)

func TestEncryptDecryptRoundTrip(t *testing.T) {
	id, err := GenerateIdentity()
	if err != nil {
		t.Fatalf("gen: %v", err)
	}
	ct, err := Encrypt("hunter2", []string{id.Recipient()})
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	if string(ct) == "hunter2" {
		t.Fatal("ciphertext must not be the plaintext")
	}
	got, err := id.Decrypt(ct)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if got != "hunter2" {
		t.Fatalf("round trip: got %q", got)
	}
}

func TestDecryptWithWrongIdentityFails(t *testing.T) {
	a, _ := GenerateIdentity()
	b, _ := GenerateIdentity()
	ct, _ := Encrypt("secret", []string{a.Recipient()})
	if _, err := b.Decrypt(ct); err == nil {
		t.Fatal("decrypt with the wrong identity must fail")
	}
}

func TestMultiRecipientAnyCanDecrypt(t *testing.T) {
	a, _ := GenerateIdentity()
	b, _ := GenerateIdentity()
	ct, _ := Encrypt("x", []string{a.Recipient(), b.Recipient()})
	for _, id := range []Identity{a, b} {
		if v, err := id.Decrypt(ct); err != nil || v != "x" {
			t.Fatalf("recipient could not decrypt: %v %q", err, v)
		}
	}
}

func TestLoadOrGeneratePersists(t *testing.T) {
	p := filepath.Join(t.TempDir(), "age.key")
	a, err := LoadOrGenerateIdentity(p)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("identity not written: %v", err)
	}
	b, err := LoadOrGenerateIdentity(p)
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if a.Recipient() != b.Recipient() {
		t.Fatal("reload must return the same identity")
	}
}
