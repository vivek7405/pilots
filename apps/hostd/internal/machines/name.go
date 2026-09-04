package machines

import (
	"context"
	"fmt"
	"regexp"
	"strings"
)

// maxNameLen keeps a name inside a DNS label, since it becomes one.
const maxNameLen = 63

// validName is what may appear as a DNS label: lowercase alphanumerics and
// hyphens, starting and ending with an alphanumeric.
var validName = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)

// leadingPortSegment matches a name whose first segment is numeric.
//
// The router reads "<port>-<name>" as a port selector, so a machine actually
// NAMED "8080-api" would be unreachable at its own URL: the request would be
// routed to port 8080 of a machine called "api".
var leadingPortSegment = regexp.MustCompile(`^[0-9]+-`)

// validateName rejects a name that cannot work as a URL.
//
// Without this a create returns 201 and a URL that never resolves -- a dot
// makes the hostname parse fail, and a numeric first segment is swallowed as a
// port selector.
func validateName(name string) error {
	switch {
	case name == "":
		return fmt.Errorf("machines: name must not be empty")
	case len(name) > maxNameLen:
		return fmt.Errorf("machines: name must be at most %d characters", maxNameLen)
	case strings.Contains(name, "."):
		return fmt.Errorf("machines: name must not contain a dot; it becomes a single DNS label")
	case !validName.MatchString(name):
		return fmt.Errorf("machines: name must be lowercase alphanumerics and hyphens, " +
			"starting and ending with an alphanumeric")
	case leadingPortSegment.MatchString(name):
		return fmt.Errorf("machines: name must not start with a number followed by a hyphen; "+
			"that form is reserved for addressing a port, as in 8080-%s", name)
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
func (m *Manager) ensureNotReserved(name string) error {
	apiHost := m.opts.APIHostname
	if apiHost == "" {
		apiHost = "api." + m.opts.Domain
	}
	if strings.EqualFold(name+"."+m.opts.Domain, apiHost) {
		return fmt.Errorf("machines: the name %q is reserved for the control API hostname %q",
			name, apiHost)
	}
	return nil
}

// ensureNameFree rejects a name already in use.
//
// Two machines sharing a name is not a cosmetic problem: the router returns
// the first row that matches, so a second machine silently steals the first
// one's URL, and which one wins depends on row ordering. URLs are permanent,
// which they cannot be if a later create can take one away.
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
	return nil
}
