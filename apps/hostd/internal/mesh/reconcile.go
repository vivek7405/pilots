package mesh

import (
	"context"
	"fmt"
	"log/slog"
	"net/netip"
	"strings"
	"time"

	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"

	"github.com/vivek7405/pilots/hostd/internal/state"
)

// reconcileInterval bounds how stale the peer set can be if a change is missed.
// The hosts cache pushes changes as they happen, so this is a safety net
// rather than the mechanism.
const reconcileInterval = 15 * time.Second

// HostSource is the cluster's current view of its hosts.
//
// An interface so the mesh does not depend on where that view comes from --
// today a Corrosion subscription, in a test a fixed slice.
type HostSource interface {
	Hosts() []state.Host
}

// Reconcile keeps the device's peers matching the fleet until ctx is done.
//
// Driven by the hosts table, which every host writes only its own row of. A
// host that joins appears as a row and becomes a peer; one that is
// decommissioned and removed stops being one. Nothing here allocates or
// assigns: a peer's address is derived from the key in its row.
//
// bootstrap is the peer a joining host was given, and it is ALWAYS included.
// Without it a new host cannot join at all: its peers come from the hosts
// table, the hosts table arrives by gossip, and gossip rides this mesh -- so
// with an empty table there is no route to anywhere, and the table stays
// empty forever. The bootstrap peer is the one edge that is configured rather
// than discovered, and it is what breaks that circle.
//
// It stays in the set even after the table fills. Once the peer's own row
// arrives the two describe the same peer and the row's version wins on merge;
// dropping it early would cut the only link a host has while it is still the
// only link.
func Reconcile(ctx context.Context, dev *Device, hosts HostSource, selfID string, bootstrap []Peer) {
	tick := time.NewTicker(reconcileInterval)
	defer tick.Stop()

	for {
		if err := dev.Sync(withBootstrap(PeersFrom(hosts.Hosts(), selfID), bootstrap)); err != nil {
			slog.Error("could not reconcile mesh peers", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// withBootstrap adds the configured peers that the hosts table does not
// already describe.
func withBootstrap(peers []Peer, bootstrap []Peer) []Peer {
	if len(bootstrap) == 0 {
		return peers
	}
	known := make(map[wgtypes.Key]bool, len(peers))
	for _, p := range peers {
		known[p.PublicKey] = true
	}
	for _, b := range bootstrap {
		if !known[b.PublicKey] {
			peers = append(peers, b)
		}
	}
	return peers
}

// ParseBootstrapPeer reads a "<public-key>@<host>:<port>" peer.
//
// This is how a joining host is told where to find the fleet. It carries the
// public KEY, not just an address, because a mesh address is derived from a
// key one way -- there is no recovering the key from it, and without the key
// there is no tunnel.
func ParseBootstrapPeer(spec string) (Peer, error) {
	at := strings.LastIndex(spec, "@")
	if at < 0 {
		return Peer{}, fmt.Errorf("mesh: bootstrap peer %q is not <public-key>@<host>:<port>", spec)
	}

	key, err := wgtypes.ParseKey(spec[:at])
	if err != nil {
		return Peer{}, fmt.Errorf("mesh: bootstrap peer key: %w", err)
	}
	endpoint, err := netip.ParseAddrPort(spec[at+1:])
	if err != nil {
		return Peer{}, fmt.Errorf("mesh: bootstrap peer endpoint: %w", err)
	}
	return Peer{PublicKey: key, Address: AddressFor(key), Endpoint: endpoint}, nil
}

// PeersFrom turns host rows into mesh peers.
//
// A peer's mesh address is DERIVED from its public key, never read from the
// row. The row carries wg_addr too, but only as a convenience for operators
// reading the table: trusting it would let a host publish someone else's
// address and take over their traffic, since a row is only ever checked
// against the host that wrote it.
func PeersFrom(hosts []state.Host, selfID string) []Peer {
	peers := make([]Peer, 0, len(hosts))

	for _, h := range hosts {
		if h.ID == selfID || h.WGPubKey == "" {
			continue
		}
		key, err := wgtypes.ParseKey(h.WGPubKey)
		if err != nil {
			slog.Warn("host row carries an unusable mesh key; skipping it",
				"host", h.ID, "err", err)
			continue
		}

		peer := Peer{PublicKey: key, Address: AddressFor(key)}
		if h.WGAddr != "" && h.WGAddr != peer.Address.String() {
			// Not fatal -- the derived address is the one used -- but it means
			// the row was written by something that does not derive it the
			// same way, which is worth knowing before it becomes a routing
			// mystery.
			slog.Warn("host row's mesh address does not match the one its key derives to",
				"host", h.ID, "row", h.WGAddr, "derived", peer.Address)
		}

		// The endpoint is where this host's WireGuard packets go: the peer's
		// public address. A host with no public address yet is still a valid
		// peer -- it can reach us and the tunnel comes up from its side.
		if h.PublicIP != "" {
			if addr, err := netip.ParseAddr(h.PublicIP); err == nil {
				peer.Endpoint = netip.AddrPortFrom(addr, ListenPort)
			} else {
				slog.Warn("host row carries an unusable public address",
					"host", h.ID, "addr", h.PublicIP, "err", err)
			}
		}
		peers = append(peers, peer)
	}
	return peers
}
