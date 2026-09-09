package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"

	pilots "github.com/vivek7405/pilots/sdks/go"

	"github.com/vivek7405/pilots/cli/internal/out"
)

// resolveMachine turns what a person typed into a machine: an id first, then
// a name. A name that matches more than one machine is refused with the ids
// listed, because picking one silently is how the wrong machine gets
// destroyed.
func resolveMachine(ctx context.Context, client *pilots.Client, idOrName string) (*pilots.Machine, error) {
	m, err := client.Machines.Get(ctx, idOrName)
	if err == nil {
		return m, nil
	}
	if !errors.Is(err, pilots.ErrNotFound) {
		return nil, err
	}
	all, err := client.Machines.List(ctx)
	if err != nil {
		return nil, err
	}
	var named []pilots.Machine
	for _, m := range all {
		if m.Name == idOrName {
			named = append(named, m)
		}
	}
	switch len(named) {
	case 1:
		return &named[0], nil
	case 0:
		return nil, out.Failf("pilot machines ls shows what exists", "no machine with id or name %s", idOrName)
	}
	ids := make([]string, len(named))
	for i, m := range named {
		ids[i] = m.ID
	}
	return nil, out.Failf("use one of the ids", "%d machines are named %s: %s", len(named), idOrName, strings.Join(ids, ", "))
}

// resolveService is the same rule for services.
func resolveService(ctx context.Context, client *pilots.Client, idOrName string) (*pilots.Service, error) {
	s, err := client.Services.Get(ctx, idOrName)
	if err == nil {
		return s, nil
	}
	if !errors.Is(err, pilots.ErrNotFound) {
		return nil, err
	}
	all, err := client.Services.List(ctx)
	if err != nil {
		return nil, err
	}
	var named []pilots.Service
	for _, s := range all {
		if s.Name == idOrName {
			named = append(named, s)
		}
	}
	switch len(named) {
	case 1:
		return &named[0], nil
	case 0:
		return nil, out.Failf("pilot services ls shows what exists", "no service with id or name %s", idOrName)
	}
	ids := make([]string, len(named))
	for i, s := range named {
		ids[i] = s.ID
	}
	return nil, out.Failf("use one of the ids", "%d services are named %s: %s", len(named), idOrName, strings.Join(ids, ", "))
}

// ExitError carries a process exit code that is not a failure of the CLI: the
// remote command's own status from exec, or a console that ended non-zero.
// Execute exits with it and prints nothing, because the program that ran has
// already said whatever it had to say.
type ExitError struct{ Code int }

func (e *ExitError) Error() string { return fmt.Sprintf("exit status %d", e.Code) }
