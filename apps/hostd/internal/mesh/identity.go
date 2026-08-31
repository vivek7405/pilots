// Package mesh is the private network every host reaches every other host on.
//
// Corrosion's gossip and the cross-host proxy both ride it, which is what lets
// both run in plaintext: the mesh already authenticates and encrypts every
// byte, and a second layer would cost a handshake for nothing.
package mesh

import (
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"strings"

	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

// AddressFor derives a host's mesh address from its public key.
//
// DERIVED, never assigned, and that is an architectural constraint rather than
// a convenience. Handing out addresses needs something to hand them out from,
// which is a registry, which is a control plane -- and it is exactly what "add
// a host = give an IP" must not require. A freshly bootstrapped host generates
// a keypair locally and computes its own address from it, with no one to ask.
//
// The address is a ULA in fdcc::/16: the prefix, then the first 14 bytes of
// the public key. Curve25519 keys are effectively random, so collisions are
// not a practical concern at fleet scale.
func AddressFor(publicKey wgtypes.Key) netip.Addr {
	var raw [16]byte
	raw[0], raw[1] = 0xfd, 0xcc
	copy(raw[2:], publicKey[:14])
	return netip.AddrFrom16(raw)
}

// Keys is a host's mesh identity.
type Keys struct {
	Private wgtypes.Key
	Public  wgtypes.Key
}

// Address is where this host is reachable on the mesh.
func (k Keys) Address() netip.Addr { return AddressFor(k.Public) }

// NewKeys generates a fresh identity.
func NewKeys() (Keys, error) {
	priv, err := wgtypes.GeneratePrivateKey()
	if err != nil {
		return Keys{}, fmt.Errorf("mesh: generate private key: %w", err)
	}
	return Keys{Private: priv, Public: priv.PublicKey()}, nil
}

// LoadOrCreateKeys reads this host's identity, generating it on first use.
//
// The identity is the host's mesh address, so it has to survive restarts: a
// host that regenerates its key becomes a different host to the rest of the
// fleet, and every peer's route to it points somewhere that no longer answers.
func LoadOrCreateKeys(path string) (Keys, error) {
	raw, err := os.ReadFile(path)
	if err == nil {
		priv, perr := wgtypes.ParseKey(strings.TrimSpace(string(raw)))
		if perr != nil {
			return Keys{}, fmt.Errorf("mesh: %s does not hold a usable private key: %w", path, perr)
		}
		return Keys{Private: priv, Public: priv.PublicKey()}, nil
	}
	if !os.IsNotExist(err) {
		return Keys{}, fmt.Errorf("mesh: read %s: %w", path, err)
	}

	keys, err := NewKeys()
	if err != nil {
		return Keys{}, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return Keys{}, fmt.Errorf("mesh: mkdir for key: %w", err)
	}
	// 0600: this key is the host's identity on the mesh.
	if err := os.WriteFile(path, []byte(keys.Private.String()+"\n"), 0o600); err != nil {
		return Keys{}, fmt.Errorf("mesh: write %s: %w", path, err)
	}
	return keys, nil
}
