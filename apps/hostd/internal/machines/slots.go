package machines

import "github.com/vivek7405/pilots/hostd/internal/netns"

// SlotFor returns a running machine's network slot.
//
// The router needs this to dial the guest: the slot carries the host-facing
// address that the namespace's NAT rewrites to the constant guest address.
// Returns false for a machine this host is not currently running, which is how
// the router knows to wake it first.
func (m *Manager) SlotFor(machineID string) (*netns.Slot, bool) {
	fcm, ok := m.get(machineID)
	if !ok || fcm.Slot == nil {
		return nil, false
	}
	return fcm.Slot, true
}
