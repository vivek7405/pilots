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
		ID: GoldenTemplateFor("AuthenticAMD"), MemBuildID: "mem-1", RootfsBuildID: "rootfs-1",
		SnapKey: "template/abc/snap.bin", CreatedAt: 1234,
	}
	if err := s.PutTemplate(ctx, want); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetTemplate(ctx, GoldenTemplateFor("AuthenticAMD"))
	if err != nil {
		t.Fatal(err)
	}
	if *got != *want {
		t.Errorf("round trip changed the template:\n got %+v\nwant %+v", *got, *want)
	}
}

// A machine must remember which template it was built from.
//
// Its images are diffs whose unchanged ranges resolve against the parent by
// offset, so restoring against a different template returns a guest stitched
// from two machines. The host's current template is not that answer: it moves
// when the golden template is rebuilt, and a host whose cache was cleared
// mints its own with fresh ids. Losing this on a round trip means the machine
// silently becomes unrestorable everywhere.
func TestAMachineRemembersTheTemplateItWasBuiltFrom(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()

	want := &Machine{
		ID: "m-1", Name: "alpha", HostID: "host-a", State: "running",
		MemBuildID: "mem-diff", RootfsBuildID: "rootfs-diff",
		TemplateMemBuildID:    "tmpl-mem",
		TemplateRootfsBuildID: "tmpl-rootfs",
	}
	if err := s.PutMachine(ctx, want); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetMachine(ctx, "m-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.TemplateMemBuildID != want.TemplateMemBuildID ||
		got.TemplateRootfsBuildID != want.TemplateRootfsBuildID {
		t.Errorf("the pinned template did not survive a round trip: got (%q, %q), want (%q, %q)",
			got.TemplateMemBuildID, got.TemplateRootfsBuildID,
			want.TemplateMemBuildID, want.TemplateRootfsBuildID)
	}
}

// A machine's app and slot must survive the round trip: the app is what scopes
// .internal resolution and the tenant filter, and the slot is the low half of
// the machine's mesh address. A column added to the schema but forgotten in
// the column list reads back empty on every host except the writer's, which
// looks like a machine that belongs to no app.
func TestMachineCarriesItsAppAndSlot(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)

	want := &Machine{
		ID: "m_app", Name: "api", HostID: "host-a", State: "running",
		App: "shop", Slot: 7,
	}
	if err := s.PutMachine(ctx, want); err != nil {
		t.Fatalf("PutMachine: %v", err)
	}
	got, err := s.GetMachine(ctx, "m_app")
	if err != nil {
		t.Fatalf("GetMachine: %v", err)
	}
	if got.App != "shop" || got.Slot != 7 {
		t.Errorf("app/slot did not survive: app=%q slot=%d", got.App, got.Slot)
	}
}

func TestServiceRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)

	want := &Service{
		ID: "svc_1", Name: "api", App: "shop", Replicas: 2,
		Env: `{"PORT":"8080"}`, EnvSealed: "sealed:opaque", CreatedAt: 1700000000,
	}
	if err := s.PutService(ctx, want); err != nil {
		t.Fatalf("PutService: %v", err)
	}
	got, err := s.GetService(ctx, "svc_1")
	if err != nil {
		t.Fatalf("GetService: %v", err)
	}
	if *got != *want {
		t.Errorf("round trip mismatch:\n got %+v\nwant %+v", *got, *want)
	}

	if _, err := s.GetService(ctx, "svc_missing"); !errors.Is(err, ErrNotFound) {
		t.Errorf("a missing service returned %v, want ErrNotFound", err)
	}
}

// The volume a service mounts is a row in its own table, not a column on
// services -- services carries rows, and a column add on one of those is the
// cr-sqlite backfill that took fly's fleet down twice.
func TestServiceVolumeRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)

	want := &ServiceVolume{ServiceID: "svc_1", Ordinal: 1, VolumeID: "vol-1", CreatedAt: 1700000000}
	if err := s.PutServiceVolume(ctx, want); err != nil {
		t.Fatalf("PutServiceVolume: %v", err)
	}
	got, err := s.ServiceVolume(ctx, "svc_1")
	if err != nil {
		t.Fatalf("ServiceVolume: %v", err)
	}
	if *got != *want {
		t.Errorf("round trip mismatch:\n got %+v\nwant %+v", *got, *want)
	}
	if _, err := s.ServiceVolume(ctx, "svc_none"); !errors.Is(err, ErrNotFound) {
		t.Errorf("a service that mounts nothing returned %v, want ErrNotFound", err)
	}
	all, err := s.ListServiceVolumes(ctx)
	if err != nil {
		t.Fatalf("ListServiceVolumes: %v", err)
	}
	if len(all) != 1 || all[0] != *want {
		t.Errorf("ListServiceVolumes returned %+v", all)
	}
	if err := s.DeleteServiceVolumes(ctx, "svc_1"); err != nil {
		t.Fatalf("DeleteServiceVolumes: %v", err)
	}
	if _, err := s.ServiceVolume(ctx, "svc_1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("the binding survived its delete: err=%v", err)
	}
}

// The binding is written once, which is what makes the arbiter's write safe
// under last-write-wins: two writers cannot disagree about a value neither of
// them may change. A second write that took effect would let a later create
// point a service at a volume the first one is already mounting.
func TestServiceVolumeIsWriteOnce(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)

	for _, vol := range []string{"vol-1", "vol-2"} {
		if err := s.PutServiceVolume(ctx,
			&ServiceVolume{ServiceID: "svc_1", Ordinal: 1, VolumeID: vol, CreatedAt: 1}); err != nil {
			t.Fatalf("PutServiceVolume %s: %v", vol, err)
		}
	}
	got, err := s.ServiceVolume(ctx, "svc_1")
	if err != nil {
		t.Fatalf("ServiceVolume: %v", err)
	}
	if got.VolumeID != "vol-1" {
		t.Errorf("the second write took: volume_id=%q, want vol-1", got.VolumeID)
	}
}

// A service with two bindings is the invariant this whole path exists to
// keep, broken. Reading the first row silently would mount one volume and
// quietly forget the other.
func TestServiceVolumeRefusesTwoRows(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)

	for i, vol := range []string{"vol-1", "vol-2"} {
		if err := s.PutServiceVolume(ctx,
			&ServiceVolume{ServiceID: "svc_1", Ordinal: i + 1, VolumeID: vol, CreatedAt: 1}); err != nil {
			t.Fatalf("PutServiceVolume %s: %v", vol, err)
		}
	}
	_, err := s.ServiceVolume(ctx, "svc_1")
	if err == nil || !strings.Contains(err.Error(), "mounts 2 volumes") {
		t.Errorf("two bindings returned %v, want an error naming both", err)
	}
}

// A shape change to a replicated table is a new table, never an ALTER.
//
// cr-sqlite backfills every existing row when a table's columns change, and
// corrosion loads this file at agent start -- so an ALTER here is a fleet-wide
// gossip storm rather than a migration. It took fly's fleet down twice for
// ~11.5h (fly.io/infra-log/2024-11-30). The column adds this repo does make
// live in addMissingColumns, which corrosion never reads.
func TestNoAlterTableInTheSchema(t *testing.T) {
	for i, line := range strings.Split(Schema, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "--") {
			continue
		}
		if strings.Contains(strings.ToUpper(line), "ALTER TABLE") {
			t.Errorf("schema.sql:%d contains ALTER TABLE: %s", i+1, strings.TrimSpace(line))
		}
	}
}

// Tenancy rows are write-once, and that is what makes "any host may write
// this" safe. If a second write could change the org, two hosts racing could
// leave an object owned by a tenant neither of them named.
func TestTenancyIsWriteOnce(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)

	if err := s.PutTenancy(ctx, &Tenancy{ID: "m_1", OrgID: "org_1", Kind: "machine", CreatedAt: 10}); err != nil {
		t.Fatalf("PutTenancy: %v", err)
	}
	if err := s.PutTenancy(ctx, &Tenancy{ID: "m_1", OrgID: "org_2", Kind: "machine", CreatedAt: 20}); err != nil {
		t.Fatalf("PutTenancy again: %v", err)
	}

	got, err := s.GetTenancy(ctx, "m_1")
	if err != nil {
		t.Fatalf("GetTenancy: %v", err)
	}
	if got.OrgID != "org_1" {
		t.Errorf("a second write moved the object to %q; tenancy must be write-once", got.OrgID)
	}
	if _, err := s.GetTenancy(ctx, "m_absent"); !errors.Is(err, ErrNotFound) {
		t.Errorf("an object with no tenancy row returned %v, want ErrNotFound", err)
	}

	all, err := s.ListTenancy(ctx)
	if err != nil {
		t.Fatalf("ListTenancy: %v", err)
	}
	if len(all) != 1 || all[0].Kind != "machine" {
		t.Errorf("ListTenancy returned %+v", all)
	}
}

// Revoking never deletes the key row: a delete racing a replica that still
// carries the insert loses through the merge, and the credential comes back.
func TestRevocationIsATombstone(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)

	if err := s.PutAPIKey(ctx, &APIKey{Hash: "abc", OrgID: "org_1", Scopes: "admin"}); err != nil {
		t.Fatalf("PutAPIKey: %v", err)
	}
	if revoked, err := s.IsRevoked(ctx, "abc"); err != nil || revoked {
		t.Fatalf("IsRevoked before revoking = %v, %v", revoked, err)
	}
	if err := s.PutRevocation(ctx, &Revocation{Hash: "abc", RevokedAt: 100}); err != nil {
		t.Fatalf("PutRevocation: %v", err)
	}
	// Re-revoking must not move the time forward: the earliest is the true one.
	if err := s.PutRevocation(ctx, &Revocation{Hash: "abc", RevokedAt: 200}); err != nil {
		t.Fatalf("PutRevocation again: %v", err)
	}

	revoked, err := s.IsRevoked(ctx, "abc")
	if err != nil || !revoked {
		t.Errorf("IsRevoked after revoking = %v, %v", revoked, err)
	}
	// The key row itself survives, so the list can still report it.
	if _, err := s.GetAPIKeyByHash(ctx, "abc"); err != nil {
		t.Errorf("revoking deleted the key row: %v", err)
	}
	keys, err := s.ListAPIKeys(ctx, "org_1")
	if err != nil || len(keys) != 1 {
		t.Errorf("ListAPIKeys = %+v, %v", keys, err)
	}
	if keys, _ := s.ListAPIKeys(ctx, "org_other"); len(keys) != 0 {
		t.Errorf("ListAPIKeys leaked another org's keys: %+v", keys)
	}
}

func TestQuotaRoundTrips(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)

	if _, err := s.GetQuota(ctx, "org_1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("an org with no row returned %v, want ErrNotFound", err)
	}
	want := &Quota{OrgID: "org_1", MaxMachines: 5, MaxVCPUs: 10, MaxMemMiB: 2048,
		MaxVolumeGiB: 50, MaxBuilds: 1, UpdatedAt: 7}
	if err := s.PutQuota(ctx, want); err != nil {
		t.Fatalf("PutQuota: %v", err)
	}
	// Unlike tenancy, a quota HAS one logical writer, so a second write wins.
	want.MaxMachines, want.UpdatedAt = 9, 8
	if err := s.PutQuota(ctx, want); err != nil {
		t.Fatalf("PutQuota again: %v", err)
	}
	got, err := s.GetQuota(ctx, "org_1")
	if err != nil {
		t.Fatalf("GetQuota: %v", err)
	}
	if *got != *want {
		t.Errorf("read back %+v, want %+v", got, want)
	}
}
