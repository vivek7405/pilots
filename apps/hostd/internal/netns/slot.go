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
	"sync"
)

const (
	// Tap-side addresses. Constant for every slot on every host, baked into
	// the kernel cmdline and therefore into every snapshot. Changing these
	// invalidates every existing snapshot in the fleet.
	TapGuestIP  = "169.254.0.21"
	TapHostIP   = "169.254.0.22"
	TapHostCIDR = "169.254.0.20/30"

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
}

// slotForIdx derives every address from the index.
//
// Note the shift/mask: the third octet carries the high byte of idx, so with a
// 1024-slot pool host IPs run 10.11.0.1 through 10.11.3.255 and veth pairs run
// 10.12.0.2 through 10.12.7.255. Formatting these as "10.11.0.%d" works only
// while idx < 256 and then silently collides.
func slotForIdx(idx int, netnsName string) *Slot {
	vrt := idx * 2
	return &Slot{
		Idx:       idx,
		HostIP:    net.IPv4(10, 11, byte(idx>>8), byte(idx&0xff)),
		VEthIP:    net.IPv4(10, 12, byte(vrt>>8), byte(vrt&0xff)),
		VPeerIP:   net.IPv4(10, 12, byte((vrt+1)>>8), byte((vrt+1)&0xff)),
		VEthName:  fmt.Sprintf("veth-%d", idx),
		VPeerName: "eth0",
		NetnsName: netnsName,
	}
}

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
}

// NewPool returns a pool of size indices.
func NewPool(size int) *Pool {
	if size <= 0 {
		size = DefaultPoolSize
	}
	return &Pool{
		max: size,
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
			return slotForIdx(idx, machineName), nil
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
	return slotForIdx(idx, machineName), nil
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
