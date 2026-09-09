package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// labelAttempts is how many suffixed labels the allocator tries before giving
// up. Four characters of [a-z0-9] is 1.6 million values, so eight collisions
// in a row means something other than luck and a 409 is the honest answer.
const labelAttempts = 8

// errAddressSet is a patch that would change an address rather than give one.
// It is a sentinel because handleUpdateService answers 409 for it and 400 for
// every other patch refusal: asking for a second address is a conflict with
// the permanence rule, not a malformed request.
var errAddressSet = errors.New("this service already has an address, and an address is permanent")

// apiError is a refusal the caller should see, carried back to the handler so
// the allocator does not need the ResponseWriter.
//
// It holds a whole ErrorResponse rather than a code string, so every code is
// written at its construction site as a Code constant. Passing a struct field
// to WriteError would take these codes out of the closed list the moment they
// stopped being literal arguments, which is exactly what
// TestEveryWriteErrorUsesAListedCode exists to prevent.
type apiError struct {
	status int
	body   ErrorResponse
}

func (e *apiError) write(w http.ResponseWriter) { writeJSON(w, e.status, e.body) }

// allocateLabel returns the address a service will answer at.
//
// An explicit request is taken literally or refused: what the caller asked for
// is what they get, because a silently adjusted address is one they will hard
// code and then find serving something else. An empty request is minted from
// the service name, with a short random suffix when the name is taken.
//
// Uniqueness is a local read over BOTH namespaces the router serves from,
// machine names and service addresses, because the router tries them in that
// order out of one namespace. The row this lands on has exactly one writer,
// the host that minted the service id and therefore arbitrates it, so no write
// option and no coordination is needed. That is the same shape machine names
// already use: a local read for uniqueness, a single-writer row, and a
// deterministic lowest-id tie-break at read time for the cross-host race
// neither read can prevent.
func (d Deps) allocateLabel(ctx context.Context, name, explicit string) (string, *apiError) {
	taken, err := d.takenLabels(ctx)
	if err != nil {
		return "", &apiError{
			status: http.StatusServiceUnavailable,
			body: ErrorResponse{
				Error: "could not read local state to allocate an address",
				Code:  CodeUnavailable, Next: "retry",
			},
		}
	}

	if explicit != "" {
		if err := ValidateLabel(explicit); err != nil {
			return "", &apiError{
				status: http.StatusBadRequest,
				body: ErrorResponse{
					Error: "domain " + err.Error(), Code: CodeBadRequest,
					Next: "send a domain that is a usable DNS label",
				},
			}
		}
		if e := d.refuseReservedLabel(explicit); e != nil {
			return "", e
		}
		if taken[explicit] {
			return "", &apiError{
				status: http.StatusConflict,
				body: ErrorResponse{
					Error: fmt.Sprintf("the address %q is already taken by a machine or another service", explicit),
					Code:  CodeConflict,
					Next: "choose another domain, or leave it empty to have one minted " +
						"from the service name",
				},
			}
		}
		return explicit, nil
	}

	base := LabelFromName(name, MaxLabelLen)
	if e := d.refuseReservedLabel(base); e == nil && !taken[base] {
		return base, nil
	}

	// The base is cut short enough to leave room for "-" plus four characters,
	// so the suffixed label still fits in a DNS label.
	short := LabelFromName(name, MaxLabelLen-5)
	for i := 0; i < labelAttempts; i++ {
		candidate := short + "-" + LabelSuffix()
		if taken[candidate] {
			continue
		}
		if e := d.refuseReservedLabel(candidate); e != nil {
			continue
		}
		return candidate, nil
	}
	return "", &apiError{
		status: http.StatusConflict,
		body: ErrorResponse{
			Error: fmt.Sprintf("could not allocate an address for %q", name),
			Code:  CodeConflict,
			Next:  "give the service a different name, or pass an explicit domain",
		},
	}
}

// takenLabels is every label the router could already resolve.
//
// Both namespaces, because resolve reads both: a machine name first and then a
// service address. A service with no address holds nothing.
//
// A machine row is taken whatever its state, tombstone included. Excluding a
// destroyed one here would be a rule this package holds alone:
// machines.ensureNameFree scans the same rows without excluding it, and the
// router's store path matches a machine row before it ever reaches a service
// address. On a store that keeps tombstones -- the single-box SQLite one; the
// corrosion store filters them out of ListMachines already -- a label minted
// over a destroyed machine's name would be a permanent address that resolves
// to the tombstone and never to the service. Where the store does filter,
// this loop never sees the row and the rule costs nothing.
func (d Deps) takenLabels(ctx context.Context) (map[string]bool, error) {
	taken := map[string]bool{}

	rows, err := d.Store.ListMachines(ctx)
	if err != nil {
		return nil, err
	}
	for _, m := range rows {
		if m.Name != "" {
			taken[m.Name] = true
		}
	}

	services, err := d.Store.ListServices(ctx)
	if err != nil {
		return nil, err
	}
	for _, svc := range services {
		if svc.Domain != "" {
			taken[svc.Domain] = true
		}
	}
	return taken, nil
}

// refuseReservedLabel rejects the one label a tenant may not take: the one that
// would produce the control API's own hostname.
//
// dispatch claims that hostname before the workload suffix check, so a service
// holding it would own a URL it could never be reached at. Derived from the
// configured hostname rather than hardcoded, the way machines.ensureNotReserved
// does it, so an operator who moves the control API moves the reservation with
// it and one who moves it off the workload domain frees the name.
func (d Deps) refuseReservedLabel(label string) *apiError {
	apiHost := d.APIHostname
	if apiHost == "" {
		apiHost = "api." + d.Domain
	}
	if !strings.EqualFold(label+"."+d.Domain, apiHost) {
		return nil
	}
	return &apiError{
		status: http.StatusBadRequest,
		body: ErrorResponse{
			Error: fmt.Sprintf("the address %q is reserved for the control API hostname %q",
				label, apiHost),
			Code: CodeBadRequest, Next: "choose another domain",
		},
	}
}
