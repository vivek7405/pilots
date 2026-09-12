package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"time"

	"github.com/vivek7405/pilots/hostd/internal/state"
)

// Choosing which host runs a new machine.
//
// # Why the receiving host decides
//
// Every host serves the whole API, so a create arrives wherever the client
// happened to send it. Running it there is what the fleet did before: machines
// piled onto whichever host the CLI was pointed at, and a fleet of five hosts
// behaved like one host with four spares.
//
// So the host that receives a create ranks the fleet from its own replica and
// offers the machine to the best candidate. No coordinator is involved and
// none is needed, because this decision is not authoritative: the candidate
// admits the machine against its own free memory or refuses with 507, and the
// receiver moves to the next. Two hosts ranking two creates from slightly
// different replicas cost each other one forward, never a double-booked host.
//
// # Why it stops after a few tries
//
// A fleet that is genuinely full should say so quickly. Walking every host on
// a large fleet to discover that would turn one create into dozens of
// round-trips, so the receiver tries a bounded number of candidates and then
// serves the request itself -- where its own admission gives the caller a
// straight answer rather than a timeout.

// maxPlacementCandidates bounds how many hosts a create is offered to before
// this host answers it itself.
const maxPlacementCandidates = 3

// placementTimeout bounds one offer. Generous enough for a create that has to
// reclaim memory before it can answer, short enough that three refusals do not
// out-wait the client.
const placementTimeout = 90 * time.Second

// Placement counts where creates went, so an operator can see whether the
// fleet is spreading or whether every create is being served locally because
// no candidate would take it.
type Placement interface {
	// Observe records one outcome: "local", "forwarded", "fallback" or
	// "refused".
	Observe(outcome string)
}

// rankForCreate orders the fleet for one create, best host first.
//
// Reads the store rather than a subscription cache on purpose: a create is
// tens per minute, not the request hot path, and four small queries are
// cheaper to reason about than a fourth cache to keep coherent.
//
// Returns nil when this host should simply handle the request itself, which is
// every single-box fleet and every case where the rows cannot be read. A
// ranking is an optimisation; failing a create because the ranking failed
// would make the fleet less reliable than no ranking at all.
func (d Deps) rankForCreate(ctx context.Context, req CreateMachineRequest, exclude []string) []string {
	hosts, err := d.Store.ListHosts(ctx)
	if err != nil || len(hosts) < 2 {
		return nil
	}
	live := make([]state.Host, 0, len(hosts))
	for _, h := range hosts {
		if time.Since(time.Unix(h.LastSeen, 0)) < liveWindow {
			live = append(live, h)
		}
	}
	if len(live) < 2 {
		return nil
	}

	capsList, err := d.Store.ListHostCapacity(ctx)
	if err != nil {
		return nil
	}
	caps := make(map[string]state.HostCapacity, len(capsList))
	for _, c := range capsList {
		caps[c.HostID] = c
	}

	// Best effort, both of them. Cached builds only feed a tie-break, and
	// vendors only matter when the create restores an image.
	cached := map[string][]string{}
	if rows, err := d.Store.ListHostBuilds(ctx); err == nil {
		for _, b := range rows {
			cached[b.HostID] = b.Builds
		}
	}
	vendors := map[string]string{}
	if rows, err := d.Store.ListHostCPU(ctx); err == nil {
		for _, c := range rows {
			vendors[c.HostID] = c.Vendor
		}
	}

	return state.RankHosts(state.PlacementRequest{
		Name:    req.Name,
		VCPUs:   orInt(req.VCPUs, 1),
		MemMiB:  orInt(req.MemMiB, 512),
		Vendor:  d.restoreVendor(ctx, req, vendors),
		Builds:  buildsNeeded(req),
		Exclude: exclude,
	}, time.Now(), live, caps, cached, vendors, liveWindow)
}

// restoreVendor is the CPU vendor a create is locked to, or empty when it can
// go anywhere.
//
// A create that RESTORES a memory image is locked: a Firecracker snapshot
// carries raw CPUID and never loads on the other vendor. A create that boots
// from an image is not. Getting this wrong in the permissive direction sends
// the machine somewhere it fails at snapshot load, naming nothing about CPUs.
func (d Deps) restoreVendor(ctx context.Context, req CreateMachineRequest, vendors map[string]string) string {
	id := req.MemBuildID
	if id == "" {
		id = req.Checkpoint
	}
	if id == "" {
		// No memory image named. A create from the golden template restores
		// too, but each host keeps its own template in its own pool, so there
		// is nothing to lock to.
		return ""
	}
	cpu, err := d.Store.GetMachineCPU(ctx, id)
	if err != nil || cpu == nil {
		// Unknown. Empty rather than a guess: an unnecessary restriction would
		// refuse hosts that could have run it, and the receiving host's own
		// restore path already checks the vendor before it loads anything.
		return ""
	}
	return cpu.Vendor
}

// buildsNeeded is the set a host would have to download if it did not already
// hold it.
func buildsNeeded(req CreateMachineRequest) []string {
	var out []string
	for _, id := range []string{req.MemBuildID, req.RootfsBuildID, req.Image} {
		if id != "" {
			out = append(out, id)
		}
	}
	return out
}

// forwardCreate offers a create to the best-ranked hosts, in order.
//
// Returns true when the request was answered -- by a candidate that took it,
// or by an error worth reporting. False means this host should serve it.
func (d Deps) forwardCreate(w http.ResponseWriter, r *http.Request, req CreateMachineRequest) bool {
	// A forwarded create never ranks again. Two hosts with briefly disagreeing
	// views would otherwise pass one create back and forth until it timed out.
	if r.Header.Get(forwardedHeader) != "" {
		return false
	}
	if d.Peers == nil {
		return false
	}

	ranked := d.rankForCreate(r.Context(), req, nil)
	if len(ranked) == 0 {
		d.observePlacement("local")
		return false
	}

	body, err := json.Marshal(req)
	if err != nil {
		return false
	}

	tried := 0
	for _, hostID := range ranked {
		if hostID == d.HostID {
			// This host is the best place for it. Nothing to forward.
			d.observePlacement("local")
			return false
		}
		if tried >= maxPlacementCandidates {
			break
		}
		addr, ok := d.Peers.InternalAddr(hostID)
		if !ok {
			continue
		}
		tried++

		status, reply, err := d.offerCreate(r, addr, body)
		if err != nil {
			// Unreachable, or it gave up. The next candidate gets the offer;
			// this is exactly the case the ranking cannot predict.
			continue
		}
		if status == http.StatusInsufficientStorage {
			// It ranked well and cannot hold it after all, which is what the
			// target being the authority means. Try the next.
			continue
		}
		d.observePlacement("forwarded")
		for k, vs := range reply.Header {
			for _, v := range vs {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(status)
		_, _ = w.Write(reply.Body)
		return true
	}

	// Everyone ranked refused or could not be reached. Serving it here is the
	// honest fallback: this host's own admission either takes it or gives the
	// caller a 507 that says the fleet is full, which is a straight answer
	// rather than a timeout.
	d.observePlacement("fallback")
	return false
}

// offeredReply is a candidate's answer, buffered so it can be replayed onto
// the client's connection after the decision to accept it.
type offeredReply struct {
	Header http.Header
	Body   []byte
}

// offerCreate sends one create to one host and reads its whole answer.
func (d Deps) offerCreate(r *http.Request, addr string, body []byte) (int, offeredReply, error) {
	ctx, cancel := context.WithTimeout(r.Context(), placementTimeout)
	defer cancel()

	// The query string travels too. An admin key acting as a tenant names the
	// org with ?org=, so a forwarded create that dropped it would be created
	// under the admin's own org rather than the tenant's.
	url := "http://" + addr + "/v1/machines"
	if r.URL.RawQuery != "" {
		url += "?" + r.URL.RawQuery
	}
	out, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return 0, offeredReply{}, err
	}
	// The caller's own credential travels with it, so the candidate resolves
	// the same org from its own replica of api_keys. Forwarding under the
	// fleet's peer token instead would make every forwarded create look like
	// it came from the platform rather than from the tenant.
	if auth := r.Header.Get("Authorization"); auth != "" {
		out.Header.Set("Authorization", auth)
	}
	out.Header.Set("Content-Type", "application/json")
	out.Header.Set(forwardedHeader, d.HostID)

	resp, err := placementClient.Do(out)
	if err != nil {
		return 0, offeredReply{}, err
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return 0, offeredReply{}, err
	}
	return resp.StatusCode, offeredReply{Header: resp.Header.Clone(), Body: raw}, nil
}

// placementClient is the one client used for offers. Its own, rather than the
// default, so a slow candidate cannot hold a connection open for ever.
var placementClient = &http.Client{Timeout: placementTimeout}

// observePlacement records an outcome when anything is listening.
func (d Deps) observePlacement(outcome string) {
	if d.Placement != nil {
		d.Placement.Observe(outcome)
	}
}

// liveWindow is how long a host may be silent and still be ranked. The same
// window serviceArbiter uses, so placement and arbitration agree about who is
// in the fleet.
const liveWindow = 90 * time.Second

func orInt(v, def int) int {
	if v == 0 {
		return def
	}
	return v
}
