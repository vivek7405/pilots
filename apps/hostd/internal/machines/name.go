package machines

import (
	"context"
	"fmt"
	"strings"

	"github.com/vivek7405/pilots/hostd/internal/api"
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
