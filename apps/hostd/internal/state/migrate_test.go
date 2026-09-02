package state

import "testing"

// A database created before a column was declared must gain it on the next
// Open. Schema is CREATE TABLE IF NOT EXISTS, which SQLite skips entirely for
// a table that already exists -- so without the migration a host upgraded in
// place answers "no such column: mem_build_id" to the first deploy.
func TestAnExistingDatabaseGainsNewColumns(t *testing.T) {
	dir := t.TempDir() + "/state.db"

	// A pre-5c database: releases without mem_build_id.
	old, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	sq := old.(*sqliteStore)
	if _, err := sq.db.Exec(`DROP TABLE releases`); err != nil {
		t.Fatal(err)
	}
	if _, err := sq.db.Exec(`CREATE TABLE releases (
		id TEXT NOT NULL PRIMARY KEY, service_id TEXT,
		rootfs_build_id TEXT, healthy INTEGER, created_at INTEGER)`); err != nil {
		t.Fatal(err)
	}
	old.Close()

	// Reopening must add the missing column rather than leave it broken.
	store, err := Open(dir)
	if err != nil {
		t.Fatalf("reopening an older database failed: %v", err)
	}
	defer store.Close()

	if err := store.PutRelease(t.Context(), &Release{
		ID: "rel-1", ServiceID: "svc-1", MemBuildID: "mem-1", Healthy: true,
	}); err != nil {
		t.Fatalf("writing a release to a migrated database: %v", err)
	}
	got, err := store.GetRelease(t.Context(), "rel-1")
	if err != nil || got.MemBuildID != "mem-1" {
		t.Errorf("release did not round-trip after migration: %+v, err=%v", got, err)
	}
}
