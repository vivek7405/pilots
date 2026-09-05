package main

import "os"

// reportedVendor is the CPU vendor this host tells the fleet it is.
//
// Normally the real one. The fleet gate needs a host that reports the OTHER
// vendor so it can drive a cold boot on a single-vendor rig, and that is what
// PILOT_FAULT_CPU_VENDOR does -- behind PILOT_FAULTS, following the same
// two-flag shape internal/nbd/faults.go uses and for the same reason: one flag
// set by accident on a real host would turn every wake into a cold boot, which
// is a fleet that reboots its customers' guests on every rescue.
//
// There is deliberately no PILOT_CPU_VENDOR knob in config.go. A real host with
// the wrong vendor pinned would photograph memory images nobody can restore, and
// unlike this it would not announce itself on /v1/health.
func reportedVendor(real string) (vendor string, forced bool) {
	if os.Getenv("PILOT_FAULTS") != "1" {
		return real, false
	}
	forcedVendor := os.Getenv("PILOT_FAULT_CPU_VENDOR")
	if forcedVendor == "" {
		return real, false
	}
	return forcedVendor, true
}
