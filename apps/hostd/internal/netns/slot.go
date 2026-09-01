// Package netns builds the per-machine network namespace.
//
// The design that matters: every guest sees the SAME network. Its eth0 is
// always 169.254.0.21 and its gateway is always 169.254.0.22, on every machine
// and every host in the fleet. Slot-specific addressing exists only in the
// namespace's NAT rules, which are rebuilt from scratch on every restore.
//
// That is what makes a memory snapshot host-agnostic: nothing host-specific is
// ever captured inside it, so a machine can be restored onto any host in the
// fleet and keep its identity.
package netns

import (
	"fmt"
	"net"
	"net/netip"
	"sync"

	"github.com/vivek7405/pilots/hostd/internal/mesh"
)

const (
	// Tap-side addresses. Constant for every slot on every host, baked into
	// the kernel cmdline and therefore into every snapshot. Changing these
	// invalidates every existing snapshot in the fleet.
	TapGuestIP  = "169.254.0.21"
	TapHostIP   = "169.254.0.22"
	TapHostCIDR = "169.254.0.20/30"

	// The IPv6 half of the same link, and constant for exactly the same
	// reason: a guest reaches its peers over IPv6 because the mesh is IPv6,
	// and an address that differed per machine or per host would be captured
	// in the snapshot and be wrong the moment the machine moved.
	//
	// The guest's own address is the same on every machine in the fleet, so it
	// carries no information -- which is why the translation to something
	// routable happens outside the guest, in namespace state that is rebuilt
	// on every restore.
	TapGuestIP6  = "fdee::21"
	TapHostIP6   = "fdee::22"
	TapHost6CIDR = "fdee::20/126"

	// Veth6Network is the per-slot IPv6 link between a namespace and the root
	// namespace: one /127 per slot, the v6 counterpart of VrtNetworkCIDR.
	Veth6Network = "fdee:1::/32"

	// Slot-specific ranges. These live only in namespace NAT rules.
	HostNetworkCIDR = "10.11.0.0/16" // one /32 per slot, the SNAT target
	VrtNetworkCIDR  = "10.12.0.0/16" // one /31 veth pair per slot

	// TapName is the tap device inside the namespace, referenced by
	// Firecracker's network-interface config as host_dev_name.
	TapName = "vmnet"

	// GuestAppPort and GuestAgentPort are what the router dials through the
	// slot's host-facing IP.
	GuestAppPort   = 8080
	GuestAgentPort = 3001
)

// DefaultPoolSize is the number of concurrent machines a host can address.
const DefaultPoolSize = 1024

// ErrPoolFull is returned when every slot index is taken.
var ErrPoolFull = fmt.Errorf("netns: slot pool full")

// Slot is one machine's network identity on this host.
type Slot struct {
	Idx int

	// HostIP is the slot's host-facing address (10.11.x.y). The router dials
	// this; namespace DNAT rewrites it to the constant guest address.
	HostIP net.IP

	// VEthIP is the host end of the veth pair, VPeerIP the namespace end.
	VEthIP  net.IP
	VPeerIP net.IP

	VEthName  string // host-side interface
	VPeerName string // namespace-side interface, always "eth0"
	NetnsName string

	// VEth6IP and VPeer6IP are the IPv6 ends of the same veth pair. The root
	// namespace routes this machine's mesh address at VPeer6IP.
	VEth6IP  netip.Addr
	VPeer6IP netip.Addr

	// Machine6 is this machine's address on the mesh: the host's derived
	// machine prefix with the slot index in its low 16 bits. Zero when the
	// host has no mesh identity, in which case the namespace gets no IPv6 at
	// all rather than half of it.
	Machine6 netip.Addr
}

// VEthNameFor is the host-side interface name for a slot index.
//
// Exported because the root-namespace tenant filter classifies traffic by the
// interface it arrived on -- every guest sources from the same 169.254.0.21,
// so the ingress veth is the only thing that identifies the sender.
func VEthNameFor(idx int) string { return fmt.Sprintf("veth-%d", idx) }

// slotForIdx derives every address from the index.
//
// Note the shift/mask: the third octet carries the high byte of idx, so with a
// 1024-slot pool host IPs run 10.11.0.1 through 10.11.3.255 and veth pairs run
// 10.12.0.2 through 10.12.7.255. Formatting these as "10.11.0.%d" works only
// while idx < 256 and then silently collides.
func slotForIdx(idx int, netnsName string, machinePrefix netip.Prefix) *Slot {
	vrt := idx * 2
	s := &Slot{
		Idx:       idx,
		HostIP:    net.IPv4(10, 11, byte(idx>>8), byte(idx&0xff)),
		VEthIP:    net.IPv4(10, 12, byte(vrt>>8), byte(vrt&0xff)),
		VPeerIP:   net.IPv4(10, 12, byte((vrt+1)>>8), byte((vrt+1)&0xff)),
		VEthName:  VEthNameFor(idx),
		VPeerName: "eth0",
		NetnsName: netnsName,
		VEth6IP:   veth6(vrt),
		VPeer6IP:  veth6(vrt + 1),
	}
	if machinePrefix.IsValid() {
		// An error here means the slot index cannot be represented in the
		// address, which the pool's own bounds already rule out. Leaving
		// Machine6 zero is the honest outcome either way: no address, and
		// therefore no IPv6 in the namespace, rather than a plausible-looking
		// one that aliases another machine.
		if addr, err := mesh.MachineAddrIn(machinePrefix, idx); err == nil {
			s.Machine6 = addr
		}
	}
	return s
}

// veth6 is the nth address in Veth6Network. Pairs are (2i, 2i+1), matching the
// v4 scheme so that a slot's two families are read off the same index.
func veth6(n int) netip.Addr {
	var raw [16]byte
	raw[0], raw[1] = 0xfd, 0xee
	raw[3] = 0x01
	raw[14], raw[15] = byte(n>>8), byte(n)
	return netip.AddrFrom16(raw)
}

// HasMesh reports whether this slot can carry guest-to-guest traffic. False on
// a host with no mesh identity, where there is nothing to reach.
func (s *Slot) HasMesh() bool { return s.Machine6.IsValid() && s.VEth6IP.IsValid() }

// VEth6CIDR and VPeer6CIDR return the IPv6 veth addresses as /127s.
func (s *Slot) VEth6CIDR() string  { return s.VEth6IP.String() + "/127" }
func (s *Slot) VPeer6CIDR() string { return s.VPeer6IP.String() + "/127" }

// HostIPCIDR returns the host-facing IP as a /32.
func (s *Slot) HostIPCIDR() string { return s.HostIP.String() + "/32" }

// VEthCIDR and VPeerCIDR return the veth pair addresses as /31s.
func (s *Slot) VEthCIDR() string  { return s.VEthIP.String() + "/31" }
func (s *Slot) VPeerCIDR() string { return s.VPeerIP.String() + "/31" }

// AppAddr and AgentAddr are what the router dials from the host namespace.
func (s *Slot) AppAddr() string   { return fmt.Sprintf("%s:%d", s.HostIP, GuestAppPort) }
func (s *Slot) AgentAddr() string { return fmt.Sprintf("%s:%d", s.HostIP, GuestAgentPort) }

// NetnsPath is the bind-mounted namespace handle, which is also what gets
// passed to the jailer as --netns.
func (s *Slot) NetnsPath() string { return "/var/run/netns/" + s.NetnsName }

// Pool hands out slot indices for this host.
type Pool struct {
	mu      sync.Mutex
	max     int
	nextIdx int
	inUse   map[int]string // idx -> machine name

	// machinePrefix is this host's block of mesh addresses. Held here rather
	// than passed to each Setup so that a slot cannot exist without the
	// address derived from it: the index and the address are the same fact.
	machinePrefix netip.Prefix
}

// NewPool returns a pool of size indices, addressing machines inside
// machinePrefix. A zero prefix means this host has no mesh identity, and its
// slots get no mesh address.
func NewPool(size int, machinePrefix netip.Prefix) *Pool {
	if size <= 0 {
		size = DefaultPoolSize
	}
	return &Pool{
		max:           size,
		machinePrefix: machinePrefix,
		// Index 0 is the unallocated sentinel: a zero-valued Slot must never
		// look like a real allocation.
		nextIdx: 1,
		inUse:   make(map[int]string),
	}
}

// Take allocates the next free index, scanning round-robin from the last one.
func (p *Pool) Take(machineName string) (*Slot, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	for i := 0; i < p.max-1; i++ {
		idx := p.nextIdx
		p.nextIdx++
		if p.nextIdx >= p.max {
			p.nextIdx = 1
		}
		if _, taken := p.inUse[idx]; !taken {
			p.inUse[idx] = machineName
			return slotForIdx(idx, machineName, p.machinePrefix), nil
		}
	}
	return nil, ErrPoolFull
}

// Reserve re-claims a specific index. This is the reconcile entry point: after
// a hostd restart the machines are still running, and their slots must be
// marked in use before anything new is handed out.
//
// Idempotent by design -- reconcile may legitimately run against a pool that
// already knows about the slot.
func (p *Pool) Reserve(idx int, machineName string) (*Slot, error) {
	if idx <= 0 || idx >= p.max {
		return nil, fmt.Errorf("netns: slot index %d out of range [1,%d)", idx, p.max)
	}
	p.mu.Lock()
	defer p.mu.Unlock()

	if owner, taken := p.inUse[idx]; taken && owner != machineName {
		return nil, fmt.Errorf("netns: slot %d already held by %q", idx, owner)
	}
	p.inUse[idx] = machineName
	return slotForIdx(idx, machineName, p.machinePrefix), nil
}

// Return releases an index.
func (p *Pool) Return(idx int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.inUse, idx)
}

// InUse reports how many indices are allocated. The e2e churn test asserts
// this returns to zero, which is how a slot leak surfaces.
func (p *Pool) InUse() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.inUse)
}
