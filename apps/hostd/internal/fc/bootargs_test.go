package fc

import (
	"strings"
	"testing"
)

// A machine booting its own image must be told what to run as PID 1.
//
// Rewriting /sbin/init inside the image cannot work: the fixups are appended
// to the build's tar, and GNU tar keeps the FIRST symlink entry for a path and
// silently ignores any later one, with or without --overwrite. So an alpine
// image kept busybox as PID 1, busybox read its own inittab, and the guest
// looped on "can't run /sbin/openrc" with the agent sitting unused inside it,
// answering nothing on its port.
// This is also what makes the guest agent's PID-1 gate correct. The agent
// configures eth0's IPv6 only when it is init, and for a BUILT image that
// holds solely because the kernel is told so here -- a base carrying systemd
// would otherwise boot systemd, which the build does not enable to configure
// the link and for which it writes no .network file, leaving a machine that
// serves with no fdee::21 and no route to its peers.
func TestInitPathReachesTheKernelCommandLine(t *testing.T) {
	args := bootArgsFor(Config{InitPath: "/opt/pilot-agent/guest-agent"})
	if !strings.Contains(args, " init=/opt/pilot-agent/guest-agent") {
		t.Errorf("the init override is absent: %s", args)
	}
}

// The golden template's /sbin/init is already what it should be, and naming an
// init for every machine would override systemd on all of them.
func TestOrdinaryMachinesGetNoInitOverride(t *testing.T) {
	if got := bootArgsFor(Config{}); strings.Contains(got, "init=") {
		t.Errorf("an ordinary machine was given an init override: %s", got)
	}
	if strings.Contains(BootArgs, "init=") {
		t.Error("the shared boot args name an init; that belongs to built images only")
	}
}
