package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/vivek7405/pilots/hostd/internal/state"
)

// Restricted keys: what a key handed to a coding agent may do, beyond its
// scopes.
//
// A scope answers "which routes", which is the right question for an operator
// and the wrong one for an agent somebody authorised on a consent screen five
// minutes ago. What that person actually chose was narrower: this agent, these
// machines, this long. These three limits are what make that choice something
// the FLEET enforces rather than something the dashboard remembers.
//
//   - `expires_at` is checked on every authenticated request, beside the
//     revocation check, because a key that outlives its lifetime is the same
//     failure as one that outlives its revocation.
//   - `name_prefix` is checked where a name is CHOSEN -- a machine create, a
//     service create -- rather than on every read. A restricted key still
//     sees the org's other machines, which is deliberate: the restriction is
//     on what it can make and change, and pretending rows do not exist would
//     make `list_machines` lie.
//   - `max_machines` is counted from the org's own rows at create time. A
//     running total would be a number to maintain, and a maintained number
//     drifts; the rows are already local and already read.
//
// A key with no limits row is unrestricted, which is every key an operator
// mints and every key that predates the table.

// limitsFor reads the caller's limits from the local replica. A key with no
// row is unrestricted, which is the common case and answers nil.
func (d Deps) limitsFor(ctx context.Context) (*state.APIKeyLimits, error) {
	hash := BearerHash(ctx)
	if hash == "" || d.Store == nil {
		return nil, nil
	}
	l, err := d.Store.GetAPIKeyLimits(ctx, hash)
	if errors.Is(err, state.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return l, nil
}

// checkNamePrefix refuses a name a restricted key may not take.
//
// An EMPTY name is allowed through: hostd mints one from the id, and a minted
// name cannot be a way to reach another agent's machine because it did not
// exist before this call. The refusal names the prefix, because an agent that
// is told only "refused" will retry the same name.
func (d Deps) checkNamePrefix(w http.ResponseWriter, r *http.Request, name, kind string) bool {
	if name == "" {
		return true
	}
	l, err := d.limitsFor(r.Context())
	if err != nil {
		writeMapped(w, err)
		return false
	}
	if l == nil || l.NamePrefix == "" {
		return true
	}
	if strings.HasPrefix(name, l.NamePrefix) {
		return true
	}
	WriteError(w, http.StatusForbidden, CodeScopeRequired,
		"this token may only name a "+kind+" starting with "+l.NamePrefix,
		"name it "+l.NamePrefix+name+", or use a token with no name restriction",
		map[string]any{"name_prefix": l.NamePrefix, "name": name})
	return false
}

// checkMachineCap refuses a create that would take a restricted key past the
// number of machines its consent screen allowed.
//
// Counted over the acting org's machines that carry the prefix, because those
// are exactly the ones this key could have made. A destroyed machine does not
// count: the row survives as a tombstone (see schema.sql) and a cap that
// counted tombstones would shrink to zero over a day's work.
func (d Deps) checkMachineCap(w http.ResponseWriter, r *http.Request) bool {
	l, err := d.limitsFor(r.Context())
	if err != nil {
		writeMapped(w, err)
		return false
	}
	if l == nil || l.MaxMachines <= 0 {
		return true
	}
	rows, err := d.Store.ListMachines(r.Context())
	if err != nil {
		writeMapped(w, err)
		return false
	}
	org := actingOrg(r)
	live := 0
	for _, m := range rows {
		if m.State == "destroyed" {
			continue
		}
		if l.NamePrefix != "" && !strings.HasPrefix(m.Name, l.NamePrefix) {
			continue
		}
		if org != "" && d.orgOf(r.Context(), m.ID) != org {
			continue
		}
		live++
	}
	if live < l.MaxMachines {
		return true
	}
	WriteError(w, http.StatusTooManyRequests, CodeQuotaExceeded,
		"this token may hold at most "+itoa(l.MaxMachines)+" machines at once",
		"destroy one of its machines, or use a token with a higher limit",
		map[string]any{"quota": "token_machines", "limit": l.MaxMachines, "used": live, "scope": "token"})
	return false
}

// orgOf is the tenancy lookup the cap uses, through the same cached view every
// authenticated request already reads.
func (d Deps) orgOf(ctx context.Context, id string) string {
	org, _ := d.tenancy().OrgOf(ctx, id)
	return org
}

// expired reports whether a key's lifetime has run out.
func expired(l *state.APIKeyLimits, now time.Time) bool {
	return l != nil && l.ExpiresAt > 0 && now.Unix() >= l.ExpiresAt
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
