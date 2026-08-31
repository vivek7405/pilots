package procname

import (
	"os"
	"strings"
	"testing"
)

// The name pkill matches is /proc/<pid>/comm, which is the MAIN thread's --
// not whichever thread the goroutine happened to be on. A rename that lands
// elsewhere changes nothing and reports no error, which is how the handlers
// kept answering to the daemon's name.
func TestSetChangesTheNamePkillMatches(t *testing.T) {
	if err := Set("pilot-test"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	raw, err := os.ReadFile("/proc/self/comm")
	if err != nil {
		t.Fatalf("read comm: %v", err)
	}
	if got := strings.TrimSpace(string(raw)); got != "pilot-test" {
		t.Errorf("comm is %q, want \"pilot-test\"", got)
	}
}

// TASK_COMM_LEN is 16 including the terminator, and the kernel truncates
// silently. A name that gets cut is a name pkill will not match, so it is
// rejected here rather than discovered in an incident.
func TestSetRejectsAnOverlongName(t *testing.T) {
	if err := Set("a-name-far-too-long-for-the-kernel"); err == nil {
		t.Error("an overlong name was accepted and would have been truncated")
	}
}
