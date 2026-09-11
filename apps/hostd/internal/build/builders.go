package build

import "context"

// Builders creates or wakes the builder machine one org's builds run in, and
// returns the address buildctl should dial.
//
// The implementation is machines.Manager. It is reached through an interface
// because the machines package already imports this one, for the path the
// guest agent is injected at inside an image, so a direct import the other way
// is a cycle.
//
// The contract is LOCAL. An implementation may look only at this host's own
// rows and may create only on this host. It must not search the fleet for a
// builder, forward the build to a host that has one, or wait on a scheduler:
// a build is served entirely by the host that received it, and the moment that
// stops being true POST /v1/builds depends on a specific machine being alive.
type Builders interface {
	// EnsureBuilder returns the buildctl address for orgID's builder on this
	// host, and a release the caller must run when the build ends. The
	// release drops the in-flight count that stops the idle monitor
	// suspending the daemon in the middle of a solve.
	EnsureBuilder(ctx context.Context, orgID string) (addr string, release func(), err error)
}
