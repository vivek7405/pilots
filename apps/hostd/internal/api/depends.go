package api

import (
	"context"
	"log/slog"
	"maps"
	"regexp"
	"slices"
	"strings"

	"github.com/vivek7405/pilots/hostd/internal/state"
)

// internalRef matches a <name>.internal address inside an environment value.
//
// The label rules are DNS's own, the ones the resolver enforces, so a value
// the resolver could never answer for cannot produce an edge here either.
//
// The TRAILING class is the one that carries weight: without it,
// "db.internalfoo" reports a dependency on db, and that is a hostname nothing
// on this fleet resolves. The leading class keeps a hyphenated label whole.
// Neither is what stops a suffix like "mydb.internal" from reading as "db" --
// the greedy capture already does that, since Go's regexp takes the leftmost
// match and the label class consumes "mydb" before ".internal" is required.
var internalRef = regexp.MustCompile(`(?i)(?:^|[^a-z0-9-])([a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?)\.internal(?:[^a-z0-9-]|$)`)

// dependsOn is the sorted set of siblings this service's environment dials.
//
// BOTH halves are read. A real database URL is a secret, so an app whose
// services find each other in exactly the way this exists to draw would
// produce an empty canvas from the plaintext half alone.
//
// Nothing decrypted leaves this function: the blob is opened, scanned for
// names, zeroed and dropped. An Open that fails logs the service id and
// nothing else -- never the blob, never the error's own text, which on a wrong
// key can quote what it could not parse -- and the derivation falls back to
// the plaintext half, which is what a host with no key does too.
//
// Only siblings count. A name matching nothing in the app is dropped rather
// than drawn: an environment may reference an address that is not a service of
// this fleet, and an edge to a card that does not exist is worse than no edge.
func (d Deps) dependsOn(svc state.Service, siblings map[string]bool) []string {
	if len(siblings) == 0 {
		return nil
	}
	self := strings.ToLower(svc.Name)
	found := map[string]bool{}
	scan := func(text string) {
		for _, m := range internalRef.FindAllStringSubmatch(text, -1) {
			name := strings.ToLower(m[1])
			// Never itself. A service that reads its own address out of its
			// own environment does not depend on itself, and a self-edge is a
			// cycle the layout would have to break for no reason.
			if name == self || !siblings[name] {
				continue
			}
			found[name] = true
		}
	}
	scan(svc.Env)
	if svc.EnvSealed != "" && d.FleetKey != nil && d.FleetKey.IsSet() {
		raw, err := d.FleetKey.Open(svc.EnvSealed)
		if err != nil {
			slog.Warn("could not read a service's sealed environment for its edges",
				"service", svc.ID)
		} else {
			scan(string(raw))
			clear(raw)
		}
	}
	if len(found) == 0 {
		return nil
	}
	return slices.Sorted(maps.Keys(found))
}

// siblingKey is what makes two services siblings: one owner and one app.
//
// Both halves matter. The app is what <name>.internal resolves within, and the
// org is what stops one tenant's canvas from naming another tenant's service
// because both happened to call an app "shop".
type siblingKey struct{ org, app string }

// siblingsOf groups every service by owner and app, so a list derives its
// edges from one pass rather than one tenancy lookup per row per row.
//
// A service with no app has no siblings, and neither has one with no owner: an
// empty App is not a group, and treating it as one would join every app-less
// service on the fleet into a single canvas.
func (d Deps) siblingsOf(ctx context.Context, rows []state.Service) map[siblingKey]map[string]bool {
	out := map[siblingKey]map[string]bool{}
	for _, row := range rows {
		if row.App == "" {
			continue
		}
		org, ok := d.tenancy().OrgOf(ctx, row.ID)
		if !ok || org == "" {
			continue
		}
		k := siblingKey{org: org, app: row.App}
		if out[k] == nil {
			out[k] = map[string]bool{}
		}
		out[k][strings.ToLower(row.Name)] = true
	}
	return out
}

// withEdges fills DependsOn on a single-row read, which has no list of its own
// to derive from.
//
// A failed list leaves DependsOn nil rather than failing the read. The edges
// are a drawing; the service is the answer, and a canvas that cannot be drawn
// must not take a service page down with it.
func (d Deps) withEdges(ctx context.Context, out *Service, svc state.Service, owner string) {
	if svc.App == "" || owner == "" {
		return
	}
	rows, err := d.Store.ListServices(ctx)
	if err != nil {
		return
	}
	groups := d.siblingsOf(ctx, rows)
	out.DependsOn = d.dependsOn(svc, groups[siblingKey{org: owner, app: svc.App}])
}
