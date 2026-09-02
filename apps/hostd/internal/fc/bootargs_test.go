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
