// Package secrets is the ONLY package that imports filippo.io/age. The control
// plane uses it to encrypt to a node's recipient; the agent uses it to decrypt
// with its identity. Everything else handles opaque ciphertext bytes.
package secrets

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"filippo.io/age"
)

type Identity struct{ id *age.X25519Identity }

func GenerateIdentity() (Identity, error) {
	id, err := age.GenerateX25519Identity()
	if err != nil {
		return Identity{}, err
	}
	return Identity{id: id}, nil
}

// LoadOrGenerateIdentity reads an age identity from path, generating and
// persisting one (0600) if the file is absent. path "" yields an ephemeral,
// unpersisted identity (tests). The identity MUST persist across agent restarts
// — a fresh one cannot decrypt secrets encrypted to the old recipient.
func LoadOrGenerateIdentity(path string) (Identity, error) {
	if path == "" {
		return GenerateIdentity()
	}
	if b, err := os.ReadFile(path); err == nil {
		id, err := age.ParseX25519Identity(string(bytes.TrimSpace(b)))
		if err != nil {
			return Identity{}, fmt.Errorf("parse identity %s: %w", path, err)
		}
		return Identity{id: id}, nil
	} else if !os.IsNotExist(err) {
		return Identity{}, fmt.Errorf("read identity %s: %w", path, err)
	}
	id, err := age.GenerateX25519Identity()
	if err != nil {
		return Identity{}, fmt.Errorf("generate identity: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return Identity{}, fmt.Errorf("mkdir %s: %w", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(id.String()+"\n"), 0o600); err != nil {
		return Identity{}, fmt.Errorf("write identity %s: %w", path, err)
	}
	return Identity{id: id}, nil
}

func (i Identity) Recipient() string { return i.id.Recipient().String() }

func (i Identity) Decrypt(ciphertext []byte) (string, error) {
	r, err := age.Decrypt(bytes.NewReader(ciphertext), i.id)
	if err != nil {
		return "", err
	}
	out, err := io.ReadAll(r)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// Encrypt seals plaintext to every recipient; any one's identity can open it.
func Encrypt(plaintext string, recipients []string) ([]byte, error) {
	if len(recipients) == 0 {
		return nil, fmt.Errorf("no recipients to encrypt to")
	}
	rs := make([]age.Recipient, 0, len(recipients))
	for _, s := range recipients {
		r, err := age.ParseX25519Recipient(s)
		if err != nil {
			return nil, fmt.Errorf("parse recipient %q: %w", s, err)
		}
		rs = append(rs, r)
	}
	var buf bytes.Buffer
	w, err := age.Encrypt(&buf, rs...)
	if err != nil {
		return nil, err
	}
	if _, err := io.WriteString(w, plaintext); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
