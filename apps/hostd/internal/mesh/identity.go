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

// The two ULA spaces the fleet uses, and the split between them is the tenant
// boundary itself.
//
// HostSpace is where hostd listens. MachineSpace is where guests live, and a
// guest may address nothing outside it -- one static rule on every host, with
// nothing to reconcile and nothing that can drift as hosts are added.
//
// The alternative considered and rejected was ONE widened prefix per host with
// the host itself at ::0. That needs an enumerated deny -- block ::0 and every
// host service port inside every machine prefix -- which has to stay correct
// on every host anyone adds for the life of the fleet. It would also readdress
// every host, and this way there is no flag day: AddressFor is untouched.
var (
	HostSpace    = netip.MustParsePrefix("fdcc::/16")
	MachineSpace = netip.MustParsePrefix("fdcd::/16")
)

// MachinePrefixBits is how much of a machine address the host contributes.
// The remaining 16 bits are the netns slot index, which is what the pool
// already allocates and already keeps stable for a machine's life on a host.
const MachinePrefixBits = 112

// MachinePrefixFor derives the block of machine addresses a host owns.
//
// Derived from the host's own key for the same reason AddressFor is: an
// allocator needs somewhere to allocate from, and that is a control plane. The
// prefix is fdcd:: followed by the first 12 bytes of the public key.
//
// The cost is worth stating rather than hiding: 16 bits of key material go to
// the slot index, leaving 96 bits of derivation entropy. Curve25519 keys are
// effectively random, so a birthday collision sits past 2^48 hosts -- against
// a fleet of tens.
func MachinePrefixFor(publicKey wgtypes.Key) netip.Prefix {
	var raw [16]byte
	raw[0], raw[1] = 0xfd, 0xcd
	copy(raw[2:], publicKey[:12])
	return netip.PrefixFrom(netip.AddrFrom16(raw), MachinePrefixBits)
}

// MaxMachineSlot is the largest slot index a machine address can carry. The
// suffix is 16 bits wide, so this is far above the netns pool's size; it
// exists so that a slot index which could never be represented is refused
// here rather than silently aliasing another machine.
const MaxMachineSlot = 0xffff

// MachineAddr is the mesh address of the machine in a host's slot.
//
// Slot 0 is deliberately not addressable: it is the netns pool's unallocated
// sentinel, so a zero-valued Slot must never derive an address that looks
// real.
func MachineAddr(publicKey wgtypes.Key, slot int) (netip.Addr, error) {
	if slot <= 0 || slot > MaxMachineSlot {
		return netip.Addr{}, fmt.Errorf("mesh: slot %d is not addressable (want 1..%d)",
			slot, MaxMachineSlot)
	}
	raw := MachinePrefixFor(publicKey).Addr().As16()
	raw[14], raw[15] = byte(slot>>8), byte(slot)
	return netip.AddrFrom16(raw), nil
}

// MachinePrefix is this host's block of machine addresses.
func (k Keys) MachinePrefix() netip.Prefix { return MachinePrefixFor(k.Public) }
