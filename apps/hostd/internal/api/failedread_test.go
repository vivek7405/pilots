package api

import (
	"context"
	"errors"
	"testing"

	"github.com/pilotsrun/pilots/hostd/internal/state"
)

// errStoreUnwell is a transient store failure, not a missing row. The
// distinction is the whole subject of this file.
var errStoreUnwell = errors.New("corrosion: query timed out")

// authErrStore fails exactly the read that says who may reach a URL.
type authErrStore struct {
	state.Store
	err error
}

func (s *authErrStore) GetURLAuth(ctx context.Context, id string) (*state.URLAuth, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.Store.GetURLAuth(ctx, id)
}

// policyErrStore fails exactly the read that says how a volume is backed up.
type policyErrStore struct {
	state.Store
	err error
}

func (s *policyErrStore) GetVolumePolicy(ctx context.Context, id string) (*state.VolumePolicy, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.Store.GetVolumePolicy(ctx, id)
}

func failedReadDeps(t *testing.T) (Deps, state.Store) {
	t.Helper()
	st, err := state.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return Deps{HostID: "host-a", Store: st}, st
}

// The ENFORCING read reports that it could not answer. The RENDERING one
// answers public and says so in a log.
//
// One helper served both, and that is how an org-gated sandbox became a public
// service: handlePromote asked what the machine's gate was, a store that could
// not answer said "public", and the promote carried that answer onto the
// service -- along with every replica the service gained afterwards, which
// carry no gate of their own. A default that is harmless on a response field
// is unrecoverable on an access-control write.
//
// Collapsing urlAuthFor back into urlAuthOf reds the first assertion.
func TestAFailedGateReadIsAnErrorForEnforcementAndPublicForDisplay(t *testing.T) {
	d, base := failedReadDeps(t)
	d.Store = &authErrStore{Store: base, err: errStoreUnwell}

	if _, err := d.urlAuthFor(context.Background(), "m-1"); !errors.Is(err, errStoreUnwell) {
		t.Errorf("urlAuthFor returned %v, want the store error: a caller about to "+
			"write an access-control row must not be told 'public' by a failure", err)
	}
	if got := d.urlAuthOf(context.Background(), "m-1"); got != URLAuthPublic {
		t.Errorf("urlAuthOf = %q, want public: a response field still renders", got)
	}
}

// No row is a real answer and means public, on both paths. Without this the
// fix would refuse every promote of a machine nobody ever gated, which is
// almost all of them.
func TestNoGateRowIsAnAnswerOnBothPaths(t *testing.T) {
	d, _ := failedReadDeps(t)

	mode, err := d.urlAuthFor(context.Background(), "m-never-gated")
	if err != nil {
		t.Errorf("a machine with no gate row errored: %v", err)
	}
	if mode != "" {
		t.Errorf("mode = %q, want empty", mode)
	}
	if got := d.urlAuthOf(context.Background(), "m-never-gated"); got != URLAuthPublic {
		t.Errorf("urlAuthOf = %q, want public", got)
	}
}

// A gate that IS set is read back unchanged on both paths.
func TestAGateThatIsSetIsReadBack(t *testing.T) {
	d, st := failedReadDeps(t)
	if err := st.PutURLAuth(context.Background(), &state.URLAuth{
		ID: "m-1", Kind: "machine", Mode: URLAuthOrg, UpdatedAt: 1,
	}); err != nil {
		t.Fatal(err)
	}

	mode, err := d.urlAuthFor(context.Background(), "m-1")
	if err != nil || mode != URLAuthOrg {
		t.Errorf("urlAuthFor = %q, %v; want org", mode, err)
	}
	if got := d.urlAuthOf(context.Background(), "m-1"); got != URLAuthOrg {
		t.Errorf("urlAuthOf = %q, want org", got)
	}
}

// A store that cannot answer "how is this volume backed up" is not the same
// answer as "it is not".
//
// handleGetVolumePolicy collapsed every error into an empty policy and a 200.
// This is the surface somebody uses to check whether their backups are on, so
// a transient fault reported -- indistinguishably from the truth -- that a
// volume with a nightly schedule had none. The two readings of that are
// "panic" and "set one over the schedule that was already there".
func TestAFailedPolicyReadIsNotAnEmptyPolicy(t *testing.T) {
	_, base := failedReadDeps(t)
	failing := &policyErrStore{Store: base, err: errStoreUnwell}

	if _, err := failing.GetVolumePolicy(context.Background(), "v-1"); !errors.Is(err, errStoreUnwell) {
		t.Fatalf("the double did not fail the read: %v", err)
	}
	// And the real store answers ErrNotFound for a volume nobody set a policy
	// on, which is the case that must still render as an empty policy.
	if _, err := base.GetVolumePolicy(context.Background(), "v-unset"); !errors.Is(err, state.ErrNotFound) {
		t.Errorf("a volume with no policy answered %v, want ErrNotFound: the handler "+
			"branches on exactly that to tell 'none set' from 'could not ask'", err)
	}
}
