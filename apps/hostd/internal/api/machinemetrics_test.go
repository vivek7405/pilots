package api

import (
	"strings"
	"testing"
)

// What crosses between hosts is the exposition itself, so the writer and the
// reader have to agree exactly. They are tested against each other rather than
// against a fixture, because a fixture only proves what somebody typed once.
func TestTheExpositionSurvivesARoundTripBetweenHosts(t *testing.T) {
	samples := []MachineMetrics{
		{
			MachineID: "m_1", Name: "web-1", ServiceID: "s_1", State: "running",
			VCPUs: 2, CPUSeconds: 12.5, MemoryBytes: 64 << 20, MemoryLimitBytes: 512 << 20,
		},
		{
			MachineID: "m_2", Name: "worker", State: "suspended",
			VCPUs: 1, CPUSeconds: 3.25, MemoryBytes: 0, MemoryLimitBytes: 256 << 20,
		},
	}

	var out strings.Builder
	writeExposition(&out, samples, nil, false, nil)
	got := parseExposition(out.String())

	if len(got) != 2 {
		t.Fatalf("parsed %d samples from %d written:\n%s", len(got), len(samples), out.String())
	}
	for i, want := range samples {
		if got[i].MachineID != want.MachineID || got[i].Name != want.Name ||
			got[i].ServiceID != want.ServiceID || got[i].State != want.State ||
			got[i].VCPUs != want.VCPUs || got[i].CPUSeconds != want.CPUSeconds ||
			got[i].MemoryBytes != want.MemoryBytes ||
			got[i].MemoryLimitBytes != want.MemoryLimitBytes {
			t.Errorf("sample %d\n got %+v\nwant %+v", i, got[i], want)
		}
	}
}

// A machine name is a label VALUE, and a value can contain a comma. Splitting
// labels on every comma would cut one in half and lose the rest of the line,
// which is a machine silently missing from a scrape.
func TestALabelValueMayContainACommaWithoutLosingTheLine(t *testing.T) {
	var out strings.Builder
	writeExposition(&out, []MachineMetrics{
		{MachineID: "m_1", Name: "odd,name", State: "running", VCPUs: 1, CPUSeconds: 1},
	}, nil, false, nil)

	got := parseExposition(out.String())
	if len(got) != 1 {
		t.Fatalf("parsed %d samples:\n%s", len(got), out.String())
	}
	if got[0].Name != "odd,name" {
		t.Errorf("name = %q, want the comma intact", got[0].Name)
	}
}

// A host that did not answer must be NAMED. Dropped silently, its machines'
// usage falls to nothing, which reads as an idle machine rather than as a host
// nobody can reach -- and that is the reading that sends somebody looking in
// the wrong place.
func TestAnUnreachableHostIsANamedSeriesRatherThanASilentGap(t *testing.T) {
	var out strings.Builder
	writeExposition(&out, nil, []string{"host-3"}, false, nil)
	body := out.String()
	if !strings.Contains(body, `pilots_metrics_hosts_unreachable{host="host-3"} 1`) {
		t.Errorf("the unreachable host is not named:\n%s", body)
	}
}

// The org label costs cardinality and says nothing on a tenant key, which sees
// exactly one org by construction. It is for an admin scrape alone.
func TestTheOrgLabelIsOnlyOnAnAdminScrape(t *testing.T) {
	samples := []MachineMetrics{{MachineID: "m_1", State: "running", VCPUs: 1}}
	orgs := map[string]string{"m_1": "org_1"}

	var tenant strings.Builder
	writeExposition(&tenant, samples, nil, false, orgs)
	if strings.Contains(tenant.String(), "org=") {
		t.Errorf("a tenant scrape carries an org label:\n%s", tenant.String())
	}

	var admin strings.Builder
	writeExposition(&admin, samples, nil, true, orgs)
	if !strings.Contains(admin.String(), `org="org_1"`) {
		t.Errorf("an admin scrape carries no org label:\n%s", admin.String())
	}
}

// State is a labelled gauge rather than a number because the states are not
// ordered: running is not more than suspended, and any encoding that made it so
// would be a graph that means nothing.
func TestStateIsALabelAndNotANumber(t *testing.T) {
	var out strings.Builder
	writeExposition(&out, []MachineMetrics{
		{MachineID: "m_1", State: "suspended", VCPUs: 1},
	}, nil, false, nil)
	body := out.String()
	if !strings.Contains(body, `pilots_machine_state{machine="m_1",state="suspended"} 1`) {
		t.Errorf("state is not a label:\n%s", body)
	}
}

// Every series needs its HELP and TYPE, or a scraper treats the whole body as
// untyped and every rate over the counter silently becomes a gauge.
func TestEverySeriesIsTyped(t *testing.T) {
	var out strings.Builder
	writeExposition(&out, []MachineMetrics{{MachineID: "m_1", State: "running", VCPUs: 1}}, nil, false, nil)
	body := out.String()
	for _, name := range []string{
		"pilots_machine_cpu_seconds_total",
		"pilots_machine_memory_bytes",
		"pilots_machine_memory_limit_bytes",
		"pilots_machine_vcpus",
		"pilots_machine_state",
	} {
		if !strings.Contains(body, "# TYPE "+name+" ") {
			t.Errorf("%s has no TYPE line", name)
		}
		if !strings.Contains(body, "# HELP "+name+" ") {
			t.Errorf("%s has no HELP line", name)
		}
	}
	if !strings.Contains(body, "# TYPE pilots_machine_cpu_seconds_total counter") {
		t.Error("the CPU total is not typed as a counter, so nothing will rate it")
	}
}
