package main

import (
	"testing"

	"github.com/vivek7405/pilots/hostd/internal/mesh"
	"github.com/vivek7405/pilots/hostd/internal/netns"
)

// The guest-facing addresses now live in FOUR places: netns's constants, the
// golden rootfs's eth0.network, hostd's firewall rules, and this binary --
// which cannot import the first because it is copied into an image and built
// on its own. A test can, though, and this is the only thing standing between
// the copies and a silent drift.
//
// Drift here does not fail a build or a create. It fails .internal, months
// later, on built images only: the guest configures an address the host is not
// translating and every peer lookup resolves fine and reaches nothing.
func TestTheGuestsAddressesMatchTheHostsConstants(t *testing.T) {
	if guestIP6 != netns.TapGuestIP6+"/126" {
		t.Errorf("guestIP6 = %q, want %q; the guest would configure an address "+
			"the host does not translate", guestIP6, netns.TapGuestIP6+"/126")
	}
	if gateway6 != netns.TapHostIP6 {
		t.Errorf("gateway6 = %q, want %q; the guest would route its peers at "+
			"nobody", gateway6, netns.TapHostIP6)
	}
	if peerPrefix != mesh.MachineSpace.String() {
		t.Errorf("peerPrefix = %q, want %q; the guest would have no route to "+
			"the machines it is meant to reach", peerPrefix, mesh.MachineSpace.String())
	}
}
