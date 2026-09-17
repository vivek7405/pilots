package state

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// TestOneLivenessWindowForTheWholeFleet is the guard on DeadAfter having one
// spelling.
//
// It was five: the store's claim guard and the self-heal loop said 30s, while
// service arbitration, placement, LiveHosts, the autoscaler and the push
// handler each carried their own 90s literal. The two answers did not merely
// drift, they contradicted -- a host silent for 45 seconds was dead enough to
// have its machines claimed and alive enough to be handed a service write, so
// the write was forwarded to a host that was already powered off and came back
// 503 from the dial. That is section 13 of the fleet gate.
//
// A literal duration compared against a host's LastSeen is therefore a bug by
// construction, whatever the number is, because it is a second answer to a
// question that must have one. This test reads the tree and says so, the way
// TestTheForwardingMarkerHasOneName reads the router's source: a compile error
// cannot catch a NEW copy, only a test that goes looking for one can.
func TestOneLivenessWindowForTheWholeFleet(t *testing.T) {
	// A duration literal on either side of a comparison involving LastSeen.
	literal := regexp.MustCompile(`LastSeen.*\b\d+\s*\*\s*time\.(Second|Minute)`)

	var offenders []string
	for _, dir := range []string{"..", "../../cmd"} {
		err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") {
				return err
			}
			// Tests may build fixtures with explicit ages; the rule is about
			// the code that DECIDES.
			if strings.HasSuffix(path, "_test.go") {
				return nil
			}
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			for i, line := range strings.Split(string(b), "\n") {
				if literal.MatchString(line) {
					offenders = append(offenders,
						filepath.ToSlash(path)+":"+itoa(i+1)+": "+strings.TrimSpace(line))
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", dir, err)
		}
	}

	if len(offenders) > 0 {
		t.Errorf("a host's liveness is decided against a literal duration in %d place(s); "+
			"use state.DeadAfter (or state.LiveHosts) so the fleet has one answer:\n  %s",
			len(offenders), strings.Join(offenders, "\n  "))
	}
}

// TestDeadAfterIsTheWindowTheClaimGuardEnforces pins the value itself.
//
// 30s is not arbitrary: it is what the store re-checks a claim against at the
// moment the claim lands, so it is the only window that confers real
// authority. Raising it here without raising it there would put the fleet back
// into the state where a host can be routed work it can no longer accept.
func TestDeadAfterIsTheWindowTheClaimGuardEnforces(t *testing.T) {
	if DeadAfter != 30*time.Second {
		t.Fatalf("DeadAfter is %v, want 30s; the store's claim guard and the "+
			"self-heal loop both derive from this, so a change here silently "+
			"changes who may claim a dead host's machines", DeadAfter)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
