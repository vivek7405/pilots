package machines

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/vivek7405/pilots/hostd/internal/api"
	"github.com/vivek7405/pilots/hostd/internal/quota"
	"github.com/vivek7405/pilots/hostd/internal/state"
)

func TestValidateNameAcceptsUsableLabels(t *testing.T) {
	for _, name := range []string{
		"webapp", "amber-harbor-k3x9", "a", "a1", "2fast", "api-v2", "api-2", "apiary",
	} {
		if err := validateName(name); err != nil {
			t.Errorf("validateName(%q) rejected a usable name: %v", name, err)
		}
	}
}

// Each of these produced a create that returned 201 and a URL that never
// resolved, or one that resolved to a different machine.
func TestValidateNameRejectsUnroutableNames(t *testing.T) {
	for _, tc := range []struct{ name, why string }{
		{"", "empty"},
		{"has.dot", "a dot makes the hostname parse fail"},
		{"8080-api", "parsed as port 8080 of a machine called api"},
		{"3000-web-app", "same, with a longer name"},
		{"UPPER", "hostnames are lowercase"},
		{"-leading", "a label may not start with a hyphen"},
		{"trailing-", "a label may not end with a hyphen"},
		{"has space", "not a legal label"},
		{"under_score", "not a legal label"},
		{"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "over 63 characters"},
	} {
		if err := validateName(tc.name); err == nil {
			t.Errorf("validateName(%q) accepted it, but %s", tc.name, tc.why)
		}
	}
}

// A name that only starts with digits is fine; the reserved form is
// digits followed by a hyphen, which the router reads as a port.
func TestValidateNameAllowsLeadingDigitsWithoutHyphen(t *testing.T) {
	if err := validateName("2fast4you"); err != nil {
		t.Errorf("rejected a name that merely starts with a digit: %v", err)
	}
	if err := validateName("22-fast"); err == nil {
		t.Error("accepted a name the router would read as port 22")
	}
}

// A rescued machine must still be reachable. The host taking it over has never
// held that machine's token, and the hash on its row authenticates a caller TO
// hostd rather than hostd to the guest -- so without derivation the rescue
// succeeds and every exec into the machine returns 401: recovered in every
// visible way, and unusable.
func TestTokensAreDerivedSoAnyHostComputesTheSame(t *testing.T) {
	const secret = "fleet-secret"

	// Two hosts, same secret, nothing shared between them.
	a := &Manager{opts: Options{HostID: "host-a", AgentTokenSecret: secret}}
	b := &Manager{opts: Options{HostID: "host-b", AgentTokenSecret: secret}}

	if a.token("m-1") != b.token("m-1") {
		t.Error("two hosts derived different tokens for the same machine; a " +
			"rescued machine would answer 401 to its new owner")
	}
	if a.token("m-1") == a.token("m-2") {
		t.Error("two machines share a token")
	}
	if a.token("m-1") == "" {
		t.Fatal("empty token")
	}

	// A different fleet must not produce the same credential.
	other := &Manager{opts: Options{HostID: "host-a", AgentTokenSecret: "other-secret"}}
	if other.token("m-1") == a.token("m-1") {
		t.Error("the token does not depend on the fleet secret")
	}
}

// With no secret -- a single box -- the previous per-host behaviour stands.
func TestTokensFallBackToTheTemplatePlaceholderWithoutASecret(t *testing.T) {
	m := &Manager{opts: Options{HostID: "host-a"}}
	if got := m.token("m-unknown"); got != templateToken {
		t.Errorf("token = %q, want the template placeholder", got)
	}
}

// The template machine never goes through installToken -- it is booted once to
// be photographed -- so its guest still carries the placeholder the golden
// rootfs ships with. Deriving a token for it locks hostd out of the very
// machine it is snapshotting, and the template build fails with a 401 that
// names nothing useful.
func TestTheTemplateMachineKeepsThePlaceholderToken(t *testing.T) {
	m := &Manager{opts: Options{HostID: "host-a", AgentTokenSecret: "fleet-secret"}}

	if got := m.token("tmpl-abc123"); got != templateToken {
		t.Errorf("template machine token = %q, want the placeholder", got)
	}
	if got := m.token("m-abc123"); got == templateToken {
		t.Error("a real machine got the placeholder; it would be reachable by " +
			"anything holding the golden rootfs")
	}
}

// "api" is the one name a tenant may not take: dispatch claims
// api.<workload domain> for the control API before the workload suffix, so a
// machine of that name would own a hostname it can never be reached at.
//
// The reservation is DERIVED from the configured hostname rather than
// hardcoded to "api". An operator who moves the control API with
// PILOT_API_HOSTNAME moves what dispatch swallows, and a reservation left on
// "api" would then leave the new name takeable -- which is the whole bug this
// rule exists to prevent -- while refusing a name that now routes fine.
func TestTheReservedNameFollowsTheConfiguredAPIHostname(t *testing.T) {
	for _, tc := range []struct {
		why         string
		apiHostname string
		machine     string
		wantErr     bool
	}{
		{"the default reserves api", "", "api", true},
		{"and nothing that merely starts with it", "", "apiary", false},
		{"an override reserves its own label", "control.pilotrun.app", "control", true},
		{"and frees the one it left behind", "control.pilotrun.app", "api", false},
		{"a hostname off the workload domain reserves nothing, because " +
			"dispatch does not claim it", "api.pilots.run", "api", false},
	} {
		t.Run(tc.why, func(t *testing.T) {
			m := New(Options{Domain: "pilotrun.app", APIHostname: tc.apiHostname})
			err := m.ensureNotReserved(tc.machine, false)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("%q was accepted, but the control API answers there", tc.machine)
				}
				if !strings.Contains(err.Error(), "reserved") {
					t.Errorf("error %q does not say the name is reserved", err)
				}
				return
			}
			if err != nil {
				t.Errorf("%q was refused: %v", tc.machine, err)
			}
		})
	}
}

// A machine named after a service's address does not merely collide with it:
// the router tries machine names BEFORE service addresses, so the machine
// would take the service's URL away from every host at once. URLs are
// permanent, so the create has to be the thing that fails.
func TestEnsureNameFreeSeesServiceAddresses(t *testing.T) {
	store, err := state.Open(":memory:")
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	if err := store.PutService(ctx, &state.Service{
		ID: "s_1", Name: "shop", Domain: "shop",
	}); err != nil {
		t.Fatalf("PutService: %v", err)
	}

	m := &Manager{opts: Options{Store: store, HostID: "host-test"}}
	err = m.ensureNameFree(ctx, "shop")
	if err == nil {
		t.Fatal("a machine was allowed to take a service's address")
	}
	if !strings.Contains(err.Error(), "service's address") {
		t.Errorf("error does not say what it collided with: %v", err)
	}

	if err := m.ensureNameFree(ctx, "other"); err != nil {
		t.Errorf("a free name was refused: %v", err)
	}
}

// A tenant may not mint a machine that hostd's own machinery would then treat
// as a builder. The prefix decides three things behind the tenant's back: the
// quota loop skips the row, the idle monitor destroys it after a day
// suspended, and the build path dials it. hostd's own create is the exception,
// and it is the only one.
func TestTheBuilderPrefixIsReservedForHostd(t *testing.T) {
	m := &Manager{opts: Options{Domain: "pilotrun.app"}}

	if err := m.ensureNotReserved("builder-acme-01", false); err == nil {
		t.Fatal("a tenant was allowed to take a builder- name")
	} else if !errors.Is(err, api.ErrBadRequest) {
		// The refusal was right and its STATUS was a 500, which told the
		// caller the fleet was broken and invited a retry of a request that
		// can never work. The reason sat in `details` where nothing reads it.
		t.Errorf("a reserved name came back as %v, which the error mapper turns "+
			"into a 500; it is the caller's to fix, so it must carry "+
			"api.ErrBadRequest and surface as a 400", err)
	}
	if err := m.ensureNotReserved("builder-acme-01", true); err != nil {
		t.Fatalf("hostd's own builder create was refused: %v", err)
	}
	// The guard is the prefix, not the whole word: "build" and "builders" are
	// ordinary names a tenant may have.
	if err := m.ensureNotReserved("build", false); err != nil {
		t.Fatalf("an ordinary name was refused: %v", err)
	}
}

// BuilderName carries the host id because ensureNameFree scans the FLEET. A
// bare builder-<org> would be takeable exactly once across every host, so the
// second host to serve that org would fail its create with "the name is
// already taken" -- which reads as a tenant error and is not one.
func TestBuilderNameIsPerHostAndRoutable(t *testing.T) {
	a := BuilderName("org-abcdefghijklmnop", "host-aaaaaaaa")
	b := BuilderName("org-abcdefghijklmnop", "host-bbbbbbbb")
	if a == b {
		t.Fatalf("two hosts derived the same builder name: %q", a)
	}
	for _, name := range []string{a, b, BuilderName("", "host-aaaaaaaa")} {
		if err := validateName(name); err != nil {
			t.Fatalf("builder name %q is not usable as a DNS label: %v", name, err)
		}
		if !strings.HasPrefix(name, builderNamePrefix) {
			t.Fatalf("builder name %q lost its prefix", name)
		}
	}
	// An org-less builder is the platform's own, used to seed the shared
	// layer cache. It must not collide with an org whose id starts "shared".
	if BuilderName("", "host-aaaaaaaa") == BuilderName("sharedorg", "host-aaaaaaaa") {
		t.Fatal("the platform builder collides with an org named shared*")
	}
}

// quota cannot import machines, so it restates the prefix. If the two ever
// disagree, builders start counting against an org's machine limit again and
// a deploy fails on a limit the org never spent.
func TestBuilderPrefixMatchesQuota(t *testing.T) {
	if builderNamePrefix != quota.BuilderNamePrefix {
		t.Fatalf("machines uses %q, quota uses %q", builderNamePrefix, quota.BuilderNamePrefix)
	}
}

// The half of BuilderName that decides who a builder belongs to must be
// collision-free, because NEITHER of its inputs is a controlled shape: org ids
// are free-form at POST /v1/api-keys, and a host id defaults to the hostname.
//
// A truncated, punctuation-stripped prefix is not enough, and both ways it
// fails are silent. Two orgs sharing a name share a BUILDER, so one tenant's
// Dockerfile runs inside another's guest. Two hosts sharing one means the
// second host's create is refused by the fleet-wide ensureNameFree, and every
// build on it fails for as long as the first host's builder exists.
func TestBuilderNamesDoNotCollideOnSimilarIDs(t *testing.T) {
	const host = "host-aaaaaaaa"

	for _, tc := range []struct{ why, a, b string }{
		{"two orgs agreeing past the readable prefix",
			"customer-alpha-1", "customer-alpha-2"},
		{"the same org id written two ways", "Acme_Corp", "acme-corp"},
		{"two orgs differing only in punctuation", "acme-1", "acme1"},
	} {
		if got, other := BuilderName(tc.a, host), BuilderName(tc.b, host); got == other {
			t.Errorf("%s: %q and %q both derive %q, so they would share a builder",
				tc.why, tc.a, tc.b, got)
		}
	}

	// And the host half, where the collision is a permanent refusal rather
	// than a crossed boundary.
	for _, tc := range []struct{ why, a, b string }{
		{"two hosts agreeing past the readable prefix",
			"pilots-hel1-01", "pilots-hel1-02"},
		{"two rig nodes", "pilots-node-1", "pilots-node-2"},
	} {
		if got, other := BuilderName("org_1", tc.a), BuilderName("org_1", tc.b); got == other {
			t.Errorf("%s: %q and %q both derive %q, so the second host could never create one",
				tc.why, tc.a, tc.b, got)
		}
	}

	// Still a legal label after the digest is appended, on the longest inputs
	// either side is likely to see.
	long := BuilderName(strings.Repeat("organisation-", 8), strings.Repeat("hostname-", 8))
	if err := validateName(long); err != nil {
		t.Fatalf("a builder name from long ids is not a usable label: %v", err)
	}
}
