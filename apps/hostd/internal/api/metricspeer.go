package api

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
)

// Asking another host what its half of a tenant's machines is using.
//
// The exposition format is what crosses the wire, not JSON, and that is
// deliberate: it is the same body this route already produces, so the peer path
// needs no second encoding to keep correct, and a mismatch between the two
// shapes cannot exist because there is only one.
//
// The caller's own Authorization travels with the request. That is what makes
// the answer correctly scoped: the peer applies the SAME visibility rule to its
// own replica, so a key that cannot see a machine cannot see it by asking a
// different host. Nothing here decides what the caller may see.

// askHostForMetrics fetches one host's local samples.
func (d Deps) askHostForMetrics(ctx context.Context, r *http.Request, hostID string) ([]MachineMetrics, error) {
	if d.Peers == nil {
		return nil, fmt.Errorf("api: this host knows no peers")
	}
	addr, ok := d.Peers.InternalAddr(hostID)
	if !ok {
		return nil, fmt.Errorf("api: no host %s in this fleet", hostID)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"http://"+addr+"/v1/metrics?local=1", nil)
	if err != nil {
		return nil, err
	}
	// The marker the internal listener requires, and the caller's own bearer:
	// the peer scopes the answer itself.
	req.Header.Set(forwardedHeader, d.HostID)
	if auth := r.Header.Get("Authorization"); auth != "" {
		req.Header.Set("Authorization", auth)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("api: host %s answered %d", hostID, resp.StatusCode)
	}
	// Bounded, because this is a body from another machine and a scrape must
	// not be a way to make a host allocate without limit.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	return parseExposition(string(body)), nil
}

// parseExposition reads back what writeExposition wrote.
//
// Only the series this file emits, and only the labels it emits: anything else
// is ignored rather than guessed at. A parser that tried to be general would be
// a second Prometheus client, which is a dependency and a surface; this one
// reads exactly what its own writer produces.
func parseExposition(body string) []MachineMetrics {
	byMachine := map[string]*MachineMetrics{}
	order := []string{}

	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, rest, ok := strings.Cut(line, "{")
		if !ok {
			continue
		}
		labelText, valueText, ok := strings.Cut(rest, "} ")
		if !ok {
			continue
		}
		labels := parseLabels(labelText)
		id := labels["machine"]
		if id == "" {
			continue
		}
		m, seen := byMachine[id]
		if !seen {
			m = &MachineMetrics{MachineID: id, Name: labels["name"], ServiceID: labels["service"]}
			byMachine[id] = m
			order = append(order, id)
		}
		switch name {
		case "pilots_machine_cpu_seconds_total":
			m.CPUSeconds, _ = strconv.ParseFloat(valueText, 64)
		case "pilots_machine_memory_bytes":
			m.MemoryBytes, _ = strconv.ParseInt(valueText, 10, 64)
		case "pilots_machine_memory_limit_bytes":
			m.MemoryLimitBytes, _ = strconv.ParseInt(valueText, 10, 64)
		case "pilots_machine_vcpus":
			n, _ := strconv.Atoi(valueText)
			m.VCPUs = n
		case "pilots_machine_state":
			// Only the state whose gauge is 1. The writer emits one line per
			// machine, but reading the value rather than assuming it means a
			// future writer that emits zeroes cannot make every machine look
			// like it is in every state at once.
			if valueText == "1" {
				m.State = labels["state"]
			}
		}
	}

	out := make([]MachineMetrics, 0, len(order))
	for _, id := range order {
		out = append(out, *byMachine[id])
	}
	return out
}

// parseLabels reads `a="b",c="d"`.
func parseLabels(text string) map[string]string {
	out := map[string]string{}
	for _, pair := range splitLabels(text) {
		key, value, ok := strings.Cut(pair, "=")
		if !ok {
			continue
		}
		unquoted, err := strconv.Unquote(value)
		if err != nil {
			continue
		}
		out[strings.TrimSpace(key)] = unquoted
	}
	return out
}

// splitLabels splits on commas OUTSIDE quotes, because a machine name is a
// label value and a value containing a comma would otherwise split a label in
// half and lose the rest of the line.
func splitLabels(text string) []string {
	var out []string
	depth := false
	current := strings.Builder{}
	for _, ch := range text {
		switch {
		case ch == '"':
			depth = !depth
			current.WriteRune(ch)
		case ch == ',' && !depth:
			out = append(out, current.String())
			current.Reset()
		default:
			current.WriteRune(ch)
		}
	}
	if current.Len() > 0 {
		out = append(out, current.String())
	}
	return out
}
