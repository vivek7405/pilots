package machines

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/vivek7405/pilots/hostd/internal/api"
	"github.com/vivek7405/pilots/hostd/internal/quota"
)

// validateName rejects a name that cannot work as a URL.
//
// The rules live in api.ValidateLabel because a service address is checked by
// the same ones: the router serves both out of one namespace, so a string that
// is legal for a machine and not for a service would be reachable depending on
// which kind of thing happened to hold it.
func validateName(name string) error {
	if err := api.ValidateLabel(name); err != nil {
		return fmt.Errorf("machines: %w", err)
	}
	return nil
}

// ensureNotReserved rejects the one name a tenant may not take: the label
// that would produce the control API's own hostname.
//
// dispatch claims that hostname before the workload suffix check, so a machine
// holding it would own a URL it could never be reached at. Derived from the
// configured hostname rather than hardcoded to "api": an operator who moves
// the control API with PILOT_API_HOSTNAME moves the reservation with it, and
// one who moves it off the workload domain entirely frees the name -- nothing
// claims it there, so it routes like any other machine.
func (m *Manager) ensureNotReserved(name string, internal bool) error {
	apiHost := m.opts.APIHostname
	if apiHost == "" {
		apiHost = "api." + m.opts.Domain
	}
	if strings.EqualFold(name+"."+m.opts.Domain, apiHost) {
		return fmt.Errorf("%w: the name %q is reserved for the control API hostname %q",
			api.ErrBadRequest, name, apiHost)
	}
	// The builder prefix is hostd's, not a tenant's. It is not cosmetic: the
	// idle monitor decides what to destroy after a day by reading this prefix,
	// and the quota loop skips these rows, so a tenant able to mint one could
	// hand itself a machine that never counts and is reaped behind its back.
	// Only the create hostd makes for itself may use it.
	if !internal && strings.HasPrefix(name, builderNamePrefix) {
		return fmt.Errorf("%w: names beginning %q are reserved for build machines",
			api.ErrBadRequest, builderNamePrefix)
	}
	return nil
}

// builderNamePrefix marks a machine as this host's builder for one org.
//
// The prefix is load-bearing in three places: ensureNotReserved refuses it to
// tenants, the quota loop does not count rows carrying it, and the idle
// monitor destroys one that has been suspended for a day. It is defined in
// the quota package and aliased here, so those three cannot disagree: quota
// cannot import this package, because this package imports quota.
const builderNamePrefix = quota.BuilderNamePrefix

// IsBuilder reports whether a machine name is a builder hostd made for itself.
//
// Exported because the router has to refuse these rows and cannot reach the
// unexported constant. The prefix is the one signal four things read: this,
// the quota loop, the idle monitor's collector, and which template a create
// restores from.
func IsBuilder(name string) bool { return strings.HasPrefix(name, builderNamePrefix) }

// BuilderName is the name of the builder machine serving one org ON THIS HOST.
//
// The host id is in the name deliberately. ensureNameFree scans the whole
// fleet, so a bare builder-<org> is takeable exactly once across every host:
// the second host to serve that org would fail its create with "the name is
// already taken", which reads as a tenant error and is not one. Builders are
// per host by design, so the name says so.
//
// An empty org is the platform's own builder, which exists only to seed the
// shared layer cache and is never handed a tenant's context. "shared" carries
// no digest, and cannot be reached by an org either: nameLabel always ends in
// six hex characters, and "shared" is not hex.
func BuilderName(orgID, hostID string) string {
	who := "shared"
	if orgID != "" {
		who = nameLabel(orgID, 12)
	}
	return builderNamePrefix + who + "-" + nameLabel(hostID, 8)
}

// nameLabel reduces an id to something legal in a DNS label: a readable prefix
// of at most n characters, then six hex of a digest of the WHOLE id.
//
// The digest is the load-bearing half, and it is not decoration. NEITHER input
// here is a controlled shape -- org ids are free-form at POST /v1/api-keys
// (see api.AllowRepo on what that has already cost), and a host id defaults to
// the hostname -- so a truncated, punctuation-stripped prefix collides on
// perfectly ordinary names, and both collisions are silent:
//
//   - Two orgs called `customer-alpha-1` and `customer-alpha-2` would derive
//     one builder name on a host, EnsureBuilder would find the other tenant's
//     row, and one org's Dockerfile and build context would run inside the
//     other's guest. That is the boundary a builder machine exists to draw.
//   - Two hosts called `pilots-hel1-01` and `pilots-hel1-02` would derive one
//     name too. findBuilder matches on host, so the second host never reuses
//     the first's row; ensureNameFree scans the FLEET, so its create is
//     refused, and every build on that host fails for as long as the first
//     host's builder exists.
//
// Six hex is 24 bits over a population of, at most, the orgs on one host, and
// a collision there is a create that is refused rather than a tenant boundary
// crossed -- the org half of the name no longer decides who anything belongs
// to on its own, because the digest covers the id in full.
func nameLabel(s string, n int) string {
	sum := sha256.Sum256([]byte(s))
	tag := hex.EncodeToString(sum[:3])

	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
		if b.Len() == n {
			break
		}
	}
	// The tag is hex, so it is alphanumeric and a legal label start on its
	// own: an id with nothing readable in it needs no placeholder character.
	return b.String() + tag
}

// ensureNameFree rejects a name already in use.
//
// Two machines sharing a name is not a cosmetic problem: the router returns
// the first row that matches, so a second machine silently steals the first
// one's URL, and which one wins depends on row ordering. URLs are permanent,
// which they cannot be if a later create can take one away.
//
// A service's address lives in the same namespace and is scanned here too. The
// router tries machine names BEFORE service addresses, so a machine named
// after a service would not merely collide with it: it would take the
// service's URL away from every host at once, which is the same permanence
// this function exists to protect.
func (m *Manager) ensureNameFree(ctx context.Context, name string) error {
	rows, err := m.opts.Store.ListMachines(ctx)
	if err != nil {
		return err
	}
	for _, row := range rows {
		if row.Name == name {
			return fmt.Errorf("machines: the name %q is already taken", name)
		}
	}
	services, err := m.opts.Store.ListServices(ctx)
	if err != nil {
		return err
	}
	for _, svc := range services {
		if svc.Domain == name {
			return fmt.Errorf("machines: the name %q is a service's address", name)
		}
	}
	return nil
}
