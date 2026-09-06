package fc

import (
	"errors"
	"testing"

	"golang.org/x/sys/unix"
)

// The slow fallback cadence belongs to exactly one cause.
//
// Every second the poll is late is a second the machine's row says running,
// the router answers 502 and the idle monitor retries a suspend against a
// corpse -- the incident this package now exists to end. Five minutes of that
// is only defensible for a kernel that genuinely cannot report an exit any
// other way. EPERM and EMFILE are faults on a host that can, and treating them
// as "no pidfd here" would quietly reintroduce the bug on a host that hit an
// fd limit once.
func TestOnlyAKernelWithoutPidfdGetsTheSlowCadence(t *testing.T) {
	if got := exitPollFor(unix.ENOSYS); got != exitPollNoPidfd {
		t.Errorf("a kernel with no pidfd_open polls every %s, want %s", got, exitPollNoPidfd)
	}
	for _, err := range []error{unix.EPERM, unix.EMFILE, unix.ENFILE,
		unix.EACCES, errors.New("something else")} {
		if got := exitPollFor(err); got != exitPollUnexpected {
			t.Errorf("%v polls every %s, want %s: this host can report exits, "+
				"so being minutes late is a choice rather than a limit",
				err, got, exitPollUnexpected)
		}
	}
}
