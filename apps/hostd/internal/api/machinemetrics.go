package api

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/vivek7405/pilots/hostd/internal/state"
)

// What a tenant's machines are using, from any host.
//
// # Why this is a second endpoint and not labels on /metrics
//
// The host's own `/metrics` is unauthenticated and label-free on purpose: a
// label per machine multiplies every series by the machine count, and a scrape
// that melts under a busy host is a scrape nobody can rely on when it matters.
// That decision is not being reversed. This is a different endpoint, with a key,
// answering a different question: not "how is this host" but "how are MY
// machines".
//
// # Why a pull across hosts and not a replicated table
//
// A `machine_stats` table would be one CRDT row per running machine per tick,
// gossiped to every host, to serve a number nobody reads until a page is open.
// Instead the local replica says WHICH machines the caller owns and where they
// run -- that read is free and never leaves this host -- and the hosts that own
// them are asked, in parallel, once.
//
// # Why an unreachable host is a line rather than a failure
//
// A fleet of any size has a host rebooting at any moment. A scrape that failed
// whole because one host was slow would be a scrape that goes blank exactly
// when somebody is looking at why a host is slow. So the answer is what could
// be read, plus a series naming what could not, which is the one shape that is
// honest AND useful.

// Stats is one sample of one machine, read from its cgroup.
//
// Declared here rather than in the machines package because the api package
// cannot import machines -- machines imports api -- and this is the value that
// crosses between them.
type Stats struct {
	// CPUSeconds is monotonic across suspend and wake: the total is persisted
	// before a cgroup is destroyed and added back afterwards, because a counter
	// that goes down makes every rate over it wrong.
	CPUSeconds float64
	// MemoryBytes is what the machine holds now. Zero while suspended, which is
	// the truth rather than a gap.
	MemoryBytes int64
	// MemoryLimitBytes is the ceiling, from the cgroup where there is one and
	// from the machine row otherwise.
	MemoryLimitBytes int64
	SampledAt        time.Time
}

// MachineMetrics is one machine's usage. GET /v1/machines/{id}/metrics.
type MachineMetrics struct {
	MachineID string `json:"machine_id"`
	Name      string `json:"name,omitempty"`
	ServiceID string `json:"service_id,omitempty"`
	State     string `json:"state"`
	VCPUs     int    `json:"vcpus"`
	MemMiB    int    `json:"mem_mib"`
	// CPUSeconds is monotonic across suspend and wake: the total is persisted
	// before a cgroup is destroyed and added back afterwards, because a counter
	// that goes down makes every rate over it wrong.
	CPUSeconds float64 `json:"cpu_seconds"`
	// MemoryBytes is what it holds now. Zero while suspended, which is the
	// truth rather than a gap.
	MemoryBytes      int64 `json:"memory_bytes"`
	MemoryLimitBytes int64 `json:"memory_limit_bytes"`
	SampledAt        int64 `json:"sampled_at"`
}

func (d Deps) handleMachineMetrics(w http.ResponseWriter, r *http.Request) {
	row, ok := d.ownedMachine(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	// The OWNER reads the cgroup, because the cgroup is on the owner. A host
	// asked about somebody else's machine forwards rather than answering with
	// zeroes, which would be a wrong number rather than a missing one.
	if d.forwardToHost(w, r, row.HostID) {
		return
	}
	got, err := d.metricsOf(r.Context(), row)
	if err != nil {
		writeMapped(w, err)
		return
	}
	writeJSON(w, http.StatusOK, got)
}

func (d Deps) metricsOf(ctx context.Context, row *state.Machine) (MachineMetrics, error) {
	out := MachineMetrics{
		MachineID: row.ID, Name: row.Name, ServiceID: row.ServiceID,
		State: row.State, VCPUs: row.VCPUs, MemMiB: row.MemMiB,
		MemoryLimitBytes: int64(row.MemMiB) << 20,
		SampledAt:        time.Now().Unix(),
	}
	if d.Machines == nil {
		return out, nil
	}
	stats, err := d.Machines.Stats(ctx, row.ID)
	if err != nil {
		return out, err
	}
	out.CPUSeconds = stats.CPUSeconds
	out.MemoryBytes = stats.MemoryBytes
	if stats.MemoryLimitBytes > 0 {
		out.MemoryLimitBytes = stats.MemoryLimitBytes
	}
	out.SampledAt = stats.SampledAt.Unix()
	return out, nil
}

// handleTenantMetrics is the scrape: every machine this key can see.
func (d Deps) handleTenantMetrics(w http.ResponseWriter, r *http.Request) {
	rows, err := d.Store.ListMachines(r.Context())
	if err != nil {
		writeMapped(w, err)
		return
	}

	// The same visibility rule the machine list applies, over the same local
	// replica, so a scrape can never show a machine a list would not.
	mine := make([]state.Machine, 0, len(rows))
	for _, row := range rows {
		if row.State == state.StateDestroyed {
			continue
		}
		if !d.mayAccess(r, row.ID) {
			continue
		}
		mine = append(mine, row)
	}

	local := r.URL.Query().Get("local") == "1"
	samples, unreachable := d.gatherMetrics(r.Context(), r, mine, local)

	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	writeExposition(w, samples, unreachable, IsAdmin(r.Context()), d.orgOfEach(r, mine))
}

// gatherMetrics reads this host's machines and asks the other hosts about
// theirs, in parallel and once each.
//
// `local` stops the recursion: a host answering a peer reports only what it
// owns, or two hosts would ask each other in a loop that ends when the context
// does.
func (d Deps) gatherMetrics(ctx context.Context, r *http.Request, rows []state.Machine,
	local bool) ([]MachineMetrics, []string) {

	byHost := map[string][]state.Machine{}
	var out []MachineMetrics
	for _, row := range rows {
		if row.HostID == "" || row.HostID == d.HostID {
			got, err := d.metricsOf(ctx, &row)
			if err == nil {
				out = append(out, got)
			}
			continue
		}
		byHost[row.HostID] = append(byHost[row.HostID], row)
	}
	if local || len(byHost) == 0 {
		return out, nil
	}

	hosts := make([]string, 0, len(byHost))
	for host := range byHost {
		hosts = append(hosts, host)
	}
	sort.Strings(hosts)

	type result struct {
		samples []MachineMetrics
		host    string
		ok      bool
	}
	results := make([]result, len(hosts))
	var wg sync.WaitGroup
	for i, host := range hosts {
		wg.Add(1)
		go func(i int, host string) {
			defer wg.Done()
			// A short deadline per host, because this is a scrape: a slow host
			// must cost one line in the answer, never the answer.
			hostCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
			defer cancel()
			samples, err := d.askHostForMetrics(hostCtx, r, host)
			results[i] = result{samples: samples, host: host, ok: err == nil}
		}(i, host)
	}
	wg.Wait()

	var unreachable []string
	for _, res := range results {
		if !res.ok {
			unreachable = append(unreachable, res.host)
			continue
		}
		out = append(out, res.samples...)
	}
	return out, unreachable
}

// orgOfEach resolves the owning org per machine, for an admin scrape.
//
// Only for an admin key: a tenant key sees one org by construction, so the
// label would be a constant on every series, and a constant label is a label
// that costs cardinality and says nothing.
func (d Deps) orgOfEach(r *http.Request, rows []state.Machine) map[string]string {
	if !IsAdmin(r.Context()) {
		return nil
	}
	out := make(map[string]string, len(rows))
	for _, row := range rows {
		if org, ok := d.tenancy().OrgOf(r.Context(), row.ID); ok {
			out[row.ID] = org
		}
	}
	return out
}

// writeExposition renders Prometheus text.
//
// Written directly rather than through the registry, because the registry's
// vectors carry one label key by design (see internal/metrics) and these carry
// three. Putting these into it would either widen every series there or add a
// second registry, and both are worse than one writer that does one thing.
func writeExposition(w io.Writer, samples []MachineMetrics, unreachable []string,
	admin bool, orgs map[string]string) {

	sort.Slice(samples, func(i, j int) bool { return samples[i].MachineID < samples[j].MachineID })

	series := []struct {
		name, help, kind string
		value            func(MachineMetrics) string
	}{
		{"pilots_machine_cpu_seconds_total", "CPU seconds used by a machine, monotonic across suspend and wake", "counter",
			func(m MachineMetrics) string { return strconv.FormatFloat(m.CPUSeconds, 'f', 3, 64) }},
		{"pilots_machine_memory_bytes", "Memory a machine holds now; zero while suspended", "gauge",
			func(m MachineMetrics) string { return strconv.FormatInt(m.MemoryBytes, 10) }},
		{"pilots_machine_memory_limit_bytes", "The memory ceiling a machine is held to", "gauge",
			func(m MachineMetrics) string { return strconv.FormatInt(m.MemoryLimitBytes, 10) }},
		{"pilots_machine_vcpus", "vCPUs a machine is allotted", "gauge",
			func(m MachineMetrics) string { return strconv.Itoa(m.VCPUs) }},
	}

	for _, s := range series {
		fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s %s\n", s.name, s.help, s.name, s.kind)
		for _, m := range samples {
			fmt.Fprintf(w, "%s{%s} %s\n", s.name, labelsFor(m, admin, orgs), s.value(m))
		}
	}

	// State as a labelled gauge rather than a number, because the states are
	// not ordered: "running" is not more than "suspended", and any encoding
	// that made it so would be a graph that means nothing.
	fmt.Fprintf(w, "# HELP pilots_machine_state Whether a machine is in a state, 1 or 0\n")
	fmt.Fprintf(w, "# TYPE pilots_machine_state gauge\n")
	for _, m := range samples {
		fmt.Fprintf(w, "pilots_machine_state{%s,state=%q} 1\n", labelsFor(m, admin, orgs), m.State)
	}

	// What could NOT be read, named. A scrape that silently dropped a host
	// would show a machine's usage falling to nothing, which reads as an idle
	// machine rather than as a host nobody can reach.
	fmt.Fprintf(w, "# HELP pilots_metrics_hosts_unreachable Hosts that did not answer this scrape\n")
	fmt.Fprintf(w, "# TYPE pilots_metrics_hosts_unreachable gauge\n")
	for _, host := range unreachable {
		fmt.Fprintf(w, "pilots_metrics_hosts_unreachable{host=%q} 1\n", host)
	}
}

func labelsFor(m MachineMetrics, admin bool, orgs map[string]string) string {
	parts := []string{fmt.Sprintf("machine=%q", m.MachineID)}
	if m.Name != "" {
		parts = append(parts, fmt.Sprintf("name=%q", m.Name))
	}
	if m.ServiceID != "" {
		parts = append(parts, fmt.Sprintf("service=%q", m.ServiceID))
	}
	if admin {
		if org := orgs[m.MachineID]; org != "" {
			parts = append(parts, fmt.Sprintf("org=%q", org))
		}
	}
	return strings.Join(parts, ",")
}
