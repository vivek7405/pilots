package state

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
)

func openTest(t *testing.T) Store {
	t.Helper()
	s, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestMachineRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)

	want := &Machine{
		ID: "m_1", Name: "webapp", HostID: "host-a", State: "running",
		KindKnobs: `{"auto_stop":"suspend","auto_start":true,"min_machines_running":0,"soft_limit":0}`,
		ImageRef:  "golden", VCPUs: 2, MemMiB: 512,
		Domain: "webapp.pilotrun.app", AppPort: 45001, AgentPort: 46001,
		AgentTokenHash: "deadbeef", LastActivity: 1700000000, UpdatedAt: 1700000001,
	}
	if err := s.PutMachine(ctx, want); err != nil {
		t.Fatalf("PutMachine: %v", err)
	}

	got, err := s.GetMachine(ctx, "m_1")
	if err != nil {
		t.Fatalf("GetMachine: %v", err)
	}
	if *got != *want {
		t.Errorf("round trip mismatch:\n got %+v\nwant %+v", *got, *want)
	}
}

// A machine's URL must survive every lifecycle transition, so an update that
// changes state must not disturb the identity columns.
func TestPutMachineUpdatePreservesIdentity(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)

	m := &Machine{ID: "m_1", Name: "webapp", HostID: "host-a", State: "running",
		Domain: "webapp.pilotrun.app", AgentTokenHash: "deadbeef", AppPort: 45001}
	if err := s.PutMachine(ctx, m); err != nil {
		t.Fatalf("PutMachine: %v", err)
	}

	m.State = "suspended"
	m.MemBuildID = "build-xyz"
	if err := s.PutMachine(ctx, m); err != nil {
		t.Fatalf("PutMachine update: %v", err)
	}

	got, err := s.GetMachine(ctx, "m_1")
	if err != nil {
		t.Fatalf("GetMachine: %v", err)
	}
	if got.State != "suspended" || got.MemBuildID != "build-xyz" {
		t.Errorf("update not applied: state=%q mem_build_id=%q", got.State, got.MemBuildID)
	}
	if got.Domain != "webapp.pilotrun.app" || got.AgentTokenHash != "deadbeef" || got.AppPort != 45001 {
		t.Errorf("identity disturbed by a state update: %+v", got)
	}
}

func TestGetMachineNotFound(t *testing.T) {
	if _, err := openTest(t).GetMachine(context.Background(), "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("got %v, want ErrNotFound", err)
	}
}

func TestListAndDeleteMachines(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)

	for _, id := range []string{"m_1", "m_2"} {
		if err := s.PutMachine(ctx, &Machine{ID: id, HostID: "host-a"}); err != nil {
			t.Fatalf("PutMachine %s: %v", id, err)
		}
	}
	list, err := s.ListMachines(ctx)
	if err != nil || len(list) != 2 {
		t.Fatalf("ListMachines: got %d rows, err %v", len(list), err)
	}

	if err := s.DeleteMachine(ctx, "m_1"); err != nil {
		t.Fatalf("DeleteMachine: %v", err)
	}
	if list, _ = s.ListMachines(ctx); len(list) != 1 {
		t.Errorf("after delete: got %d rows, want 1", len(list))
	}
}

func TestHostRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)

	if err := s.PutHost(ctx, &Host{ID: "host-a", WGAddr: "10.100.0.1",
		PublicIP: "1.2.3.4", CPUFree: 8, MemFreeMiB: 16384, LastSeen: 1700000000}); err != nil {
		t.Fatalf("PutHost: %v", err)
	}
	hosts, err := s.ListHosts(ctx)
	if err != nil || len(hosts) != 1 {
		t.Fatalf("ListHosts: got %d, err %v", len(hosts), err)
	}
	if hosts[0].WGAddr != "10.100.0.1" || hosts[0].CPUFree != 8 {
		t.Errorf("host round trip mismatch: %+v", hosts[0])
	}
}

func TestCheckpointChain(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)

	// v2 chains from v1 via SourceID; restoring an older one must stay possible.
	for i, c := range []*Checkpoint{
		{ID: "c1", MachineID: "m_1", Seq: 1, Comment: "v1", MemBuildID: "mb1", Durable: true},
		{ID: "c2", MachineID: "m_1", Seq: 2, Comment: "v2", SourceID: "c1", MemBuildID: "mb2"},
	} {
		if err := s.PutCheckpoint(ctx, c); err != nil {
			t.Fatalf("PutCheckpoint %d: %v", i, err)
		}
	}

	got, err := s.ListCheckpoints(ctx, "m_1")
	if err != nil || len(got) != 2 {
		t.Fatalf("ListCheckpoints: got %d, err %v", len(got), err)
	}
	if got[0].Seq != 1 || got[1].Seq != 2 {
		t.Errorf("checkpoints out of order: %d, %d", got[0].Seq, got[1].Seq)
	}
	if got[1].SourceID != "c1" {
		t.Errorf("chain lost: SourceID = %q, want c1", got[1].SourceID)
	}
	if !got[0].Durable || got[1].Durable {
		t.Errorf("durable flag not round-tripped: %v, %v", got[0].Durable, got[1].Durable)
	}
}

func TestAPIKeyLookup(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)

	if err := s.PutAPIKey(ctx, &APIKey{Hash: "abc123", OrgID: "org_1",
		Scopes: "machines,deploy", CreatedAt: 1700000000}); err != nil {
		t.Fatalf("PutAPIKey: %v", err)
	}
	k, err := s.GetAPIKeyByHash(ctx, "abc123")
	if err != nil {
		t.Fatalf("GetAPIKeyByHash: %v", err)
	}
	if k.OrgID != "org_1" || k.Scopes != "machines,deploy" {
		t.Errorf("api key mismatch: %+v", k)
	}
	if _, err := s.GetAPIKeyByHash(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Errorf("got %v, want ErrNotFound", err)
	}
}

// schema.sql is loaded verbatim by Corrosion from phase 4. cr-sqlite merges
// last-write-wins and cannot enforce constraints, so any that creep in would
// have to be ripped out later -- along with every row that violated them in
// the meantime. Fail here instead.
// The schema is loaded verbatim by Corrosion, so it has to satisfy
// cr-sqlite's rules -- and cr-sqlite enforces them by refusing the WHOLE
// schema at startup while still serving its API, so the symptom is every read
// returning "no such table" rather than anything that names the schema.
func TestSchemaIsCRDTSafe(t *testing.T) {
	var ddl strings.Builder
	for _, line := range strings.Split(Schema, "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "--") {
			ddl.WriteString(strings.ToUpper(line) + "\n")
		}
	}

	// A merge builds a row from columns written at different times on
	// different hosts, so nothing may constrain a row as a whole.
	for _, banned := range []string{"UNIQUE", "FOREIGN KEY", "REFERENCES", "CHECK("} {
		if strings.Contains(ddl.String(), banned) {
			t.Errorf("schema.sql contains %q: not CRDT-safe, see ARCHITECTURE.md", banned)
		}
	}

	// Every table needs a primary key, and it must be NOT NULL. cr-sqlite
	// refuses a nullable one, and in SQLite a bare `TEXT PRIMARY KEY` IS
	// nullable -- unlike INTEGER PRIMARY KEY, which aliases rowid. This is the
	// rule the schema originally got wrong, and it cost a cluster that came up
	// with every table missing.
	tables := 0
	for _, stmt := range strings.Split(ddl.String(), "CREATE TABLE") {
		if !strings.Contains(stmt, "PRIMARY KEY") {
			continue
		}
		tables++
		name := strings.Fields(strings.ReplaceAll(stmt, "IF NOT EXISTS", ""))[0]
		for _, line := range strings.Split(stmt, "\n") {
			if !strings.Contains(strings.ToUpper(line), "PRIMARY KEY") {
				continue
			}
			if !strings.Contains(strings.ToUpper(line), "NOT NULL") {
				t.Errorf("%s: primary key is nullable (%s); cr-sqlite refuses the "+
					"whole schema and every read then reports no such table",
					name, strings.TrimSpace(line))
			}
		}
	}
	if tables == 0 {
		t.Fatal("no tables found; this test is not checking anything")
	}

	// Any OTHER NOT NULL column needs a DEFAULT, because a merge can produce a
	// row before that column has ever been written.
	for _, line := range strings.Split(ddl.String(), "\n") {
		upper := strings.ToUpper(line)
		if !strings.Contains(upper, "NOT NULL") || strings.Contains(upper, "PRIMARY KEY") {
			continue
		}
		if !strings.Contains(upper, "DEFAULT") {
			t.Errorf("NOT NULL without a DEFAULT: %s", strings.TrimSpace(line))
		}
	}
}

// The golden template's parts must survive as a set.
//
// cr-sqlite merges per column, so three columns are three independent races.
// Two hosts publishing at once could leave a row pairing one host's memory
// build with the other's disk build: a template that never existed, whose
// restores resolve unchanged pages against the wrong parent. That is silent
// guest-memory corruption on every machine the fleet creates.
//
// The defence is structural -- one column -- so the test is structural too.
func TestTheTemplateDescriptorIsNotSplitAcrossColumns(t *testing.T) {
	schema, err := os.ReadFile("schema.sql")
	if err != nil {
		t.Fatal(err)
	}
	// Only the templates table: machines legitimately carry these columns.
	start := strings.Index(string(schema), "CREATE TABLE IF NOT EXISTS templates")
	if start < 0 {
		t.Fatal("no templates table in schema.sql")
	}
	body := string(schema)[start:]
	end := strings.Index(body, ");")
	if end < 0 {
		t.Fatal("templates table is unterminated")
	}
	body = body[:end]

	// Comments explain the rule and name the fields; they are not columns.
	var columns []string
	for _, line := range strings.Split(body, "\n") {
		if cut := strings.Index(line, "--"); cut >= 0 {
			line = line[:cut]
		}
		columns = append(columns, line)
	}
	declared := strings.Join(columns, "\n")

	for _, split := range []string{"mem_build_id", "rootfs_build_id", "snap_key"} {
		if strings.Contains(declared, split) {
			t.Errorf("templates has a separate %q column: the parts of a template "+
				"merge independently and can pair builds no host published", split)
		}
	}
}

// Round-tripping through the single column must not quietly lose a part.
func TestTemplateRoundTrip(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()

	want := &Template{
		ID: GoldenTemplate, MemBuildID: "mem-1", RootfsBuildID: "rootfs-1",
		SnapKey: "template/abc/snap.bin", CreatedAt: 1234,
	}
	if err := s.PutTemplate(ctx, want); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetTemplate(ctx, GoldenTemplate)
	if err != nil {
		t.Fatal(err)
	}
	if *got != *want {
		t.Errorf("round trip changed the template:\n got %+v\nwant %+v", *got, *want)
	}
}
