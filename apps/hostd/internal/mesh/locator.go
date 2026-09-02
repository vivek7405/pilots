package mesh

import (
	"log/slog"
	"net/netip"

	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"

	"github.com/vivek7405/pilots/hostd/internal/state"
)

// Locator answers where a machine is reachable on the mesh.
//
// The address is derived, never read: the owning host's public key gives the
// prefix and the machine's slot index gives the last 16 bits. Nothing
// publishes a machine address, for the same reason PeersFrom ignores
// hosts.wg_addr -- a row is only ever checked against the host that wrote it,
// so an address taken from one would let a host claim another's traffic.
//
// This host's own key is held separately rather than looked up in the hosts
// table. A single box never writes a hosts row at all, and a host that has
// just started has not yet heartbeated its own; in both cases it still knows
// its own key, and its own machines are the ones it most needs to place.
type Locator struct {
	selfID  string
	selfKey wgtypes.Key
	hosts   HostSource
}

// NewLocator returns a locator for this host's view of the fleet.
func NewLocator(selfID string, selfKey wgtypes.Key, hosts HostSource) *Locator {
	return &Locator{selfID: selfID, selfKey: selfKey, hosts: hosts}
}

// MachineAddress is where a machine's row says it can be reached, or false if
// it cannot be.
//
// A machine with no slot is not addressable, and that is the normal state of a
// suspended one: it holds no index on any host until it wakes. Answering with
// an address anyway would point traffic at whichever machine took that index
// next -- on a host that exists, over a route that works, into the wrong
// guest.
func (l *Locator) MachineAddress(m state.Machine) (netip.Addr, bool) {
	if m.Slot <= 0 || m.State == state.StateDestroyed {
		return netip.Addr{}, false
	}

	prefix, ok := l.prefixFor(m.HostID)
	if !ok {
		return netip.Addr{}, false
	}
	addr, err := MachineAddrIn(prefix, m.Slot)
	if err != nil {
		slog.Warn("a machine row names a slot that has no address",
			"machine", m.ID, "slot", m.Slot, "err", err)
		return netip.Addr{}, false
	}
	return addr, true
}

// prefixFor is the machine block a host owns.
func (l *Locator) prefixFor(hostID string) (netip.Prefix, bool) {
	if hostID == l.selfID {
		// A host that failed to load its identity holds the ZERO key, and
		// every host in that state derives the same block -- so answering
		// here would put unrelated machines on each other's addresses. The
		// same reason main.go leaves the machine prefix zero rather than
		// deriving one from a key it does not have.
		if l.selfKey == (wgtypes.Key{}) {
			return netip.Prefix{}, false
		}
		return MachinePrefixFor(l.selfKey), true
	}
	if l.hosts == nil {
		return netip.Prefix{}, false
	}
	for _, h := range l.hosts.Hosts() {
		if h.ID != hostID || h.WGPubKey == "" {
			continue
		}
		key, err := wgtypes.ParseKey(h.WGPubKey)
		if err != nil {
			// Already warned about by PeersFrom on every reconcile; a second
			// line per DNS query would drown the log.
			return netip.Prefix{}, false
		}
		return MachinePrefixFor(key), true
	}
	// A host whose row has not arrived yet. Its machines are unreachable until
	// it does, which is correct: there is no key to derive from and guessing
	// would send traffic to whoever happens to own that block.
	return netip.Prefix{}, false
}
