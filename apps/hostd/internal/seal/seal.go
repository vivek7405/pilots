// Package seal encrypts the values that must never appear in a replicated row.
//
// Corrosion gossips every row to every host, so a secret written in the clear
// to one host is a secret on all of them, in every host's database file and in
// every backup taken of one. The reference implementation gets this wrong in a
// way worth reading: uncloud stores each container as JSON embedding its
// resolved environment, so a deploy fans the plaintext out fleet-wide.
//
// The limit here is real and stating it is part of shipping it. This defends
// against gossip spread, and against anything that reads a database file or a
// backup. It does NOT defend against a compromised host: every host holds the
// fleet key, so any host can decrypt any org's secrets. Sealing per host to
// the owner's key would break self-heal, since the rescuing host would need
// plaintext it has no way to obtain. Real untrusted-host secrecy needs a KMS,
// and a KMS is a control plane.
package seal

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
)

// prefix tags the format, so a future scheme can be told from this one by
// looking at a value rather than by remembering when it was written.
const prefix = "pk1:"

// KeyBytes is the fleet key's length: AES-256.
const KeyBytes = 32

// ErrNoKey is returned when a host is asked to seal or open without one.
//
// A distinct error because the operator response is specific: the key is
// supplied out of band to host-bootstrap.sh and lives only in
// /etc/pilots/config. Nothing can recover it, and nothing else can be done.
var ErrNoKey = errors.New("seal: this host has no fleet key")

// Key is the fleet-wide sealing key.
//
// Custody is a stated exception to "object storage is the only truth for
// machine state". The key is operator-held, supplied out of band, and lives
// only in /etc/pilots/config. Wipe every host and the sealed values are
// unrecoverable with object storage completely intact -- it is the one piece
// of state whose durability is the operator's job, in the same trust class as
// the SSH key that runs the bootstrap.
type Key struct {
	aead cipher.AEAD
	set  bool
}

// ParseKey reads a base64 fleet key. An empty spec yields an unset key, which
// every operation refuses rather than silently storing plaintext.
func ParseKey(spec string) (Key, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return Key{}, nil
	}

	raw, err := base64.StdEncoding.DecodeString(spec)
	if err != nil {
		raw, err = base64.RawStdEncoding.DecodeString(spec)
	}
	if err != nil {
		return Key{}, fmt.Errorf("seal: the fleet key is not base64: %w", err)
	}
	if len(raw) != KeyBytes {
		return Key{}, fmt.Errorf("seal: the fleet key is %d bytes, want %d",
			len(raw), KeyBytes)
	}

	block, err := aes.NewCipher(raw)
	if err != nil {
		return Key{}, fmt.Errorf("seal: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return Key{}, fmt.Errorf("seal: %w", err)
	}
	return Key{aead: aead, set: true}, nil
}

// IsSet reports whether this host can seal.
func (k Key) IsSet() bool { return k.set }

// Seal encrypts plaintext for storage in a replicated row.
//
// A fresh random nonce every time, so two machines given the same secret do
// not produce the same blob -- a reader with no key could otherwise tell which
// rows share a value, which is enough to spot a shared credential.
func (k Key) Seal(plaintext []byte) (string, error) {
	if !k.set {
		return "", ErrNoKey
	}
	nonce := make([]byte, k.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("seal: nonce: %w", err)
	}
	sealed := k.aead.Seal(nonce, nonce, plaintext, nil)
	return prefix + base64.StdEncoding.EncodeToString(sealed), nil
}

// Open decrypts a blob written by Seal.
func (k Key) Open(blob string) ([]byte, error) {
	if blob == "" {
		return nil, nil
	}
	if !k.set {
		return nil, ErrNoKey
	}
	if !strings.HasPrefix(blob, prefix) {
		return nil, fmt.Errorf("seal: %q is not a sealed value", truncate(blob))
	}

	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(blob, prefix))
	if err != nil {
		return nil, fmt.Errorf("seal: sealed value is not base64: %w", err)
	}
	if len(raw) < k.aead.NonceSize() {
		return nil, errors.New("seal: sealed value is too short to hold a nonce")
	}

	nonce, ct := raw[:k.aead.NonceSize()], raw[k.aead.NonceSize():]
	plaintext, err := k.aead.Open(nil, nonce, ct, nil)
	if err != nil {
		// Almost always a key that is not the one this was sealed with, which
		// on a fleet means one host was bootstrapped with a different value.
		return nil, fmt.Errorf("seal: could not open a sealed value; this host's "+
			"fleet key is not the one it was sealed with: %w", err)
	}
	return plaintext, nil
}

// truncate keeps an error message from quoting a whole blob back at a log.
func truncate(s string) string {
	if len(s) > 16 {
		return s[:16] + "..."
	}
	return s
}
