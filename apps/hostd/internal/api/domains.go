package api

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/vivek7405/pilots/hostd/internal/state"
)

type AddDomainRequest struct {
	ServiceID string `json:"service_id"`
	Hostname  string `json:"hostname"`
}

type DomainResponse struct {
	Hostname  string `json:"hostname"`
	ServiceID string `json:"service_id"`
	Verified  bool   `json:"verified"`
	// Target is what the customer's CNAME has to point at. Returned on every
	// response, including the failure, because "verification failed" without
	// naming the expected target is a support ticket rather than an error.
	Target    string `json:"cname_target"`
	CreatedAt int64  `json:"created_at"`
}

// handleAddDomain registers a custom hostname and verifies it points here.
//
// Verification is not ceremony. certmagic will obtain a certificate on demand
// for any name this fleet admits, so admitting one the caller does not control
// spends the fleet's shared Let's Encrypt rate limit on someone else's typo --
// and the rate limit is per registered domain, so one bad entry can lock out
// every real customer.
func (d Deps) handleAddDomain(w http.ResponseWriter, r *http.Request) {
	var req AddDomainRequest
	if err := decodeBody(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: err.Error()})
		return
	}
	host := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(req.Hostname), "."))
	if host == "" || req.ServiceID == "" {
		writeJSON(w, http.StatusBadRequest,
			ErrorResponse{Error: "service_id and hostname are required"})
		return
	}
	// Our own apex is not a custom domain. Accepting one would let a caller
	// claim another tenant's workload URL.
	if strings.HasSuffix(host, "."+d.Domain) || host == d.Domain {
		writeJSON(w, http.StatusBadRequest, ErrorResponse{
			Error: fmt.Sprintf("%s is already served by this fleet; a custom domain is a name you own", host)})
		return
	}

	// Only the service's arbiter may write its domain row, so forward rather
	// than refuse -- PutDomain enforces the same rule and would otherwise 409
	// on two hosts in three.
	if d.forwardToArbiter(w, r, req.ServiceID) {
		return
	}

	svc, err := d.Store.GetService(r.Context(), req.ServiceID)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	target := svc.Domain + "." + d.Domain

	row := &state.Domain{
		Hostname: host, ServiceID: svc.ID, CreatedAt: time.Now().Unix(),
	}
	if err := VerifyHostname(r.Context(), d.Resolver, host, target); err == nil {
		row.VerifiedAt = time.Now().Unix()
	}

	if err := d.Store.PutDomain(r.Context(), row); err != nil {
		writeStoreError(w, err)
		return
	}

	resp := DomainResponse{
		Hostname: row.Hostname, ServiceID: row.ServiceID,
		Verified: row.VerifiedAt != 0, Target: target, CreatedAt: row.CreatedAt,
	}
	// Registered either way: DNS propagates on its own schedule, and refusing
	// the row would make the caller re-issue the request rather than just wait
	// for the next verification pass.
	code := http.StatusCreated
	if !resp.Verified {
		code = http.StatusAccepted
	}
	writeJSON(w, code, resp)
}

func (d Deps) handleListDomains(w http.ResponseWriter, r *http.Request) {
	rows, err := d.Store.ListDomains(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
		return
	}
	out := make([]DomainResponse, 0, len(rows))
	for _, row := range rows {
		svc, err := d.Store.GetService(r.Context(), row.ServiceID)
		target := ""
		if err == nil {
			target = svc.Domain + "." + d.Domain
		}
		out = append(out, DomainResponse{
			Hostname: row.Hostname, ServiceID: row.ServiceID,
			Verified: row.VerifiedAt != 0, Target: target, CreatedAt: row.CreatedAt,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

func (d Deps) handleDeleteDomain(w http.ResponseWriter, r *http.Request) {
	// Guarded, unlike the bare delete this used to be: DeleteDomain runs an
	// unconditional DELETE, so without an ownership check any host could
	// remove another service's hostname -- and the row would vanish fleet-wide
	// with nothing reporting who did it.
	row, err := d.Store.GetDomain(r.Context(), r.PathValue("hostname"))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if d.forwardToArbiter(w, r, row.ServiceID) {
		return
	}
	if err := d.Store.DeleteDomain(r.Context(), r.PathValue("hostname")); err != nil {
		writeStoreError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// Resolver is the DNS lookup verification uses. An interface so a test can
// answer without a nameserver.
type Resolver interface {
	LookupCNAME(ctx context.Context, host string) (string, error)
	LookupHost(ctx context.Context, host string) ([]string, error)
}

// verifyCNAME reports whether a hostname points at this fleet.
//
// A CNAME is the documented way, but an apex cannot carry one, so an A record
// pointing at an address the fleet's own name resolves to is accepted as well
// -- otherwise every customer using their root domain would be refused for
// following their registrar's rules.
func VerifyHostname(ctx context.Context, res Resolver, host, target string) error {
	if res == nil {
		res = netResolver{}
	}
	want := strings.ToLower(strings.TrimSuffix(target, "."))

	if cname, err := res.LookupCNAME(ctx, host); err == nil {
		if strings.ToLower(strings.TrimSuffix(cname, ".")) == want {
			return nil
		}
	}

	ours, err := res.LookupHost(ctx, target)
	if err != nil {
		return fmt.Errorf("cannot resolve %s to compare against: %w", target, err)
	}
	theirs, err := res.LookupHost(ctx, host)
	if err != nil {
		return fmt.Errorf("cannot resolve %s: %w", host, err)
	}
	fleet := make(map[string]struct{}, len(ours))
	for _, a := range ours {
		fleet[a] = struct{}{}
	}
	for _, a := range theirs {
		if _, ok := fleet[a]; ok {
			return nil
		}
	}
	return fmt.Errorf("%s does not point at %s", host, target)
}

type netResolver struct{}

func (netResolver) LookupCNAME(ctx context.Context, host string) (string, error) {
	return net.DefaultResolver.LookupCNAME(ctx, host)
}
func (netResolver) LookupHost(ctx context.Context, host string) ([]string, error) {
	return net.DefaultResolver.LookupHost(ctx, host)
}
