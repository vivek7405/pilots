package nbd

import "os"

// Deliberate fault injection, for the hostility battery and nothing else.
//
// The NBD wedge is the most expensive failure this package knows about: a
// handler blocked in NBD_DO_IT never runs its own cleanup, so skipping the
// disconnect ioctl leaves /dev/nbdN attached to a server that is gone,
// Firecracker stuck in D-state, and the host needing a reboot. Process.Stop
// gets the ordering right (see its comment), and an assertion that the
// ordering holds proves nothing unless the same run can show the wedge
// happening when the ordering is removed. That negative control is what these
// flags are for: scripts/cluster/gate.sh arms them on one host, reproduces the
// wedge, disarms them, and reboots that host.
//
// Two flags, both required, because one of them being set by accident on a
// real host would cost a reboot. PILOT_FAULTS is the master switch and carries
// no fault of its own; PILOT_FAULT_NBD_SKIP_DISCONNECT names this one fault.
// Neither is read anywhere else in hostd, and neither has a default that arms
// anything.

// faultsEnabled reports whether fault injection is armed at all.
func faultsEnabled() bool {
	return os.Getenv("PILOT_FAULTS") == "1"
}

// skipDisconnect reports whether Process.Stop must skip the disconnect ioctl,
// reproducing the wedge on purpose. It is true only when BOTH flags are set.
func skipDisconnect() bool {
	return faultsEnabled() && os.Getenv("PILOT_FAULT_NBD_SKIP_DISCONNECT") == "1"
}
