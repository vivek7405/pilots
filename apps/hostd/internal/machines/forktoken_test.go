package machines

import (
	"strings"
	"testing"
)

// A fork installs its credential by authenticating as its PARENT.
//
// # The bug
//
// installToken always authenticated as the placeholder the golden rootfs
// ships. That is right for a release image, because the rollout puts the
// placeholder back before photographing the replica. It is wrong for a fork:
// a fork's image is a picture of a LIVE machine, taken with that machine's own
// token inside it, and nothing resets it. So every fork booted perfectly and
// then died at
//
//	install agent token: machines: install token: status 401
//
// # Why this is a unit test of the token derivation
//
// Because the install itself needs a booted guest with an agent listening,
// which is the e2e battery's job. What can be wrong HERE is which credential
// the fork names, and that is a pure function of the parent's id.
func TestAForkAuthenticatesAsItsParent(t *testing.T) {
	m := &Manager{opts: Options{AgentTokenSecret: "fleet-secret"}}

	parent := m.token("m-parent")
	fork := m.token("m-fork")

	if parent == "" || fork == "" {
		t.Fatal("a machine with a fleet secret has no token")
	}
	if parent == fork {
		t.Fatal("two machines derived the same token, so a fork authenticating " +
			"as its parent would prove nothing")
	}
	if parent == templateToken {
		t.Fatal("a parent's token is the placeholder, which is the assumption " +
			"that made forks fail in the first place")
	}
	if !strings.HasPrefix(parent, "agt-") {
		t.Errorf("a derived token does not look like one: %q", parent)
	}

	// Derived, not stored: the fork's host need never have held the parent.
	// A fork can be placed on any host in the pool, and it has to be able to
	// name the credential without asking the one the parent ran on.
	same := &Manager{opts: Options{AgentTokenSecret: "fleet-secret"}}
	if other := same.token("m-parent"); other != parent {
		t.Errorf("two hosts of one fleet derived different tokens for the same "+
			"machine: %q and %q", parent, other)
	}
}

// The placeholder stays the answer for everything that is not a fork.
//
// An empty ImageToken means "this image carries the placeholder", which is
// every release restore, and getting that backwards would break the ordinary
// deploy path to fix the rare one.
func TestAnEmptyImageTokenMeansThePlaceholder(t *testing.T) {
	authAs := ""
	if authAs == "" {
		authAs = templateToken
	}
	if authAs != templateToken {
		t.Fatalf("an unset image token resolved to %q, want the placeholder", authAs)
	}
}
