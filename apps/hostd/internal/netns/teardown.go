package netns

import (
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/vishvananda/netlink"
	vnetns "github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
)

// ebusyRetries and ebusyDelay bound the wait for a dying Firecracker to let go
// of its namespace.
const (
	ebusyRetries = 10
	ebusyDelay   = 300 * time.Millisecond
)

// Teardown removes a slot's namespace and host-side state.
//
// Safe to call on a slot that was never set up, or was only partly set up:
// every step tolerates absence, because this runs both on the normal destroy
// path and as the first step of Setup.
func Teardown(s *Slot) error {
	var errs []error

	// Host route first. Removing it before the namespace goes away means
	// nothing can be routed at a half-torn-down slot.
	route := &netlink.Route{
		Dst: &net.IPNet{IP: s.HostIP, Mask: net.CIDRMask(32, 32)},
		Gw:  s.VPeerIP,
	}
	if err := netlink.RouteDel(route); err != nil && !isNotFound(err) {
		errs = append(errs, fmt.Errorf("route del %s: %w", s.HostIPCIDR(), err))
	}

	// Deleting the namespace takes the veth peer and the tap with it; the
	// host-side veth goes too, since a veth cannot outlive its peer.
	if err := deleteNamedNS(s.NetnsName); err != nil {
		errs = append(errs, err)
	}

	// Belt and braces: if the namespace was never created, the host-side veth
	// is still around.
	if link, err := netlink.LinkByName(s.VEthName); err == nil {
		if err := netlink.LinkDel(link); err != nil && !isNotFound(err) {
			errs = append(errs, fmt.Errorf("link del %s: %w", s.VEthName, err))
		}
	}

	return errors.Join(errs...)
}

// deleteNamedNS unlinks /var/run/netns/<name>, retrying past EBUSY.
//
// A Firecracker process that has just been SIGKILLed still holds the namespace
// fd while it is a zombie, so the delete returns EBUSY. Swallowing that error
// leaves a stale namespace behind and the NEXT create for the same machine
// fails with "file exists" -- which is how a back-to-back
// snapshot-and-kill then restore breaks.
func deleteNamedNS(name string) error {
	var lastErr error
	for attempt := 0; attempt < ebusyRetries; attempt++ {
		err := vnetns.DeleteNamed(name)
		if err == nil || isNotFound(err) {
			return nil
		}
		lastErr = err
		if !errors.Is(err, unix.EBUSY) {
			return fmt.Errorf("netns: delete %s: %w", name, err)
		}
		time.Sleep(ebusyDelay)
	}
	return fmt.Errorf("netns: delete %s still EBUSY after %d attempts: %w",
		name, ebusyRetries, lastErr)
}

// TeardownByName removes a namespace when only its name is known.
//
// The reaper finds an orphaned machine by its process, not by a slot, so it has
// no Slot to tear down -- but a namespace left behind blocks the next machine
// that lands on the same slot with "file exists".
func TeardownByName(netnsName string) error {
	return deleteNamedNS(netnsName)
}

// GCOrphanVeths removes veth-* links with no peer, left behind when a teardown
// was interrupted. Called on reconcile, so a crashed hostd does not leak
// interfaces across restarts.
func GCOrphanVeths(inUse map[string]bool) error {
	links, err := netlink.LinkList()
	if err != nil {
		return fmt.Errorf("netns: list links: %w", err)
	}
	var errs []error
	for _, l := range links {
		name := l.Attrs().Name
		if !strings.HasPrefix(name, "veth-") || inUse[name] {
			continue
		}
		if l.Type() != "veth" {
			continue
		}
		// A veth whose peer is gone has no master and no peer index; deleting
		// it is safe because a live slot's veth is always in inUse.
		if err := netlink.LinkDel(l); err != nil && !isNotFound(err) {
			errs = append(errs, fmt.Errorf("link del %s: %w", name, err))
		}
	}
	return errors.Join(errs...)
}

func isNotFound(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, unix.ENOENT) || errors.Is(err, unix.ESRCH) || errors.Is(err, unix.ENODEV) {
		return true
	}
	var linkNotFound netlink.LinkNotFoundError
	if errors.As(err, &linkNotFound) {
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, "no such file") ||
		strings.Contains(msg, "no such device") ||
		strings.Contains(msg, "not found") ||
		strings.Contains(msg, "no such process")
}
