package pilots

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A client inside a machine, with no key, uses the token the agent maintains.
func TestAClientWithNoKeyReadsTheMachinesToken(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte("pbt1.abc.def\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	t.Setenv("PILOT_TOKEN_FILE", path)

	c := New("")
	if got := c.credential(); got != "pbt1.abc.def" {
		t.Errorf("credential = %q, want the token with its newline trimmed", got)
	}
}

// An explicit key is an explicit choice and must never be quietly replaced.
// Silently preferring a machine's own token would make a process that passed a
// key act as something else, which is the worst kind of authentication bug: the
// call succeeds, against the wrong identity.
func TestAnExplicitKeyIsNeverReplacedByTheMachinesToken(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte("pbt1.machine.token"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	t.Setenv("PILOT_TOKEN_FILE", path)

	c := New("pilot_operator_key")
	if got := c.credential(); got != "pilot_operator_key" {
		t.Errorf("credential = %q, want the key that was passed", got)
	}
	if c.broker != nil {
		t.Error("a client given a key still built a broker credential")
	}
}

// No token file is the ordinary state of an ungranted machine. The client must
// send no credential rather than one saying "", and must not fail to construct.
func TestNoTokenFileIsNotAnError(t *testing.T) {
	t.Setenv("PILOT_TOKEN_FILE", filepath.Join(t.TempDir(), "absent"))
	c := New("")
	if got := c.credential(); got != "" {
		t.Errorf("credential = %q, want empty", got)
	}
}

// The token changes every five minutes, so a client must not hold one for the
// life of the process: that is a 401 with no cause anybody can see.
func TestAReplacedTokenIsPickedUp(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte("pbt1.first"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	cred := &brokerCredential{path: path}
	if got := cred.Token(); got != "pbt1.first" {
		t.Fatalf("first read = %q", got)
	}

	if err := os.WriteFile(path, []byte("pbt1.second"), 0o600); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	// The cache is deliberately short rather than absent; aged out by hand here
	// so the test does not sleep for it.
	cred.readAt = time.Now().Add(-2 * brokerTokenTTL)
	if got := cred.Token(); got != "pbt1.second" {
		t.Errorf("second read = %q, want the replaced token", got)
	}
}

// Outside a machine there is no broker, and nothing should behave as though
// there were.
func TestInsideMachineIsFalseWithNoBroker(t *testing.T) {
	t.Setenv("PILOT_BROKER_URL", "")
	if InsideMachine() {
		t.Error("InsideMachine is true with no PILOT_BROKER_URL")
	}
	t.Setenv("PILOT_BROKER_URL", "http://169.254.0.22:3002")
	if !InsideMachine() {
		t.Error("InsideMachine is false with a broker address set")
	}
}
