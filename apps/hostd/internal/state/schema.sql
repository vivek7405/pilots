-- pilots cluster state.
--
-- This schema is loaded verbatim by Corrosion, so it must stay CRDT-safe:
-- cr-sqlite merges rows last-write-wins across hosts and CANNOT enforce
-- constraints. So there are no UNIQUE, no FOREIGN KEY and no CHECK anywhere
-- below. Uniqueness (machine names, domains) comes from deterministic
-- ownership -- hash(key) mod live_hosts -- not from the database.
--
-- The one place NOT NULL is REQUIRED is the primary key. cr-sqlite refuses a
-- table whose primary key is nullable, and in SQLite a bare `TEXT PRIMARY KEY`
-- IS nullable -- unlike INTEGER PRIMARY KEY, which aliases rowid. Leaving it
-- off does not fail at write time or at review time: the agent refuses the
-- whole schema at startup, serves its API anyway, and every read comes back
-- "no such table".
--
-- Any other NOT NULL column would need a DEFAULT, because a merge can
-- construct a row from columns written at different times.
--
-- The `writer:` comments are the only in-code record of the single-writer
-- invariant: a host writes ONLY rows describing its own machines. Violating
-- it does not error, it corrupts state silently. See ARCHITECTURE.md.
--
-- A machine is never DELETEd either. A delete racing an update loses the race
-- through the merge and the row comes back, so destruction is the state
-- `destroyed` plus a reaper that collects those rows after a retention window.
-- Every read filters them.

CREATE TABLE IF NOT EXISTS hosts (      -- writer: the host itself
  id            TEXT NOT NULL PRIMARY KEY,
  wg_addr       TEXT,    -- derived from wg_pubkey; never assigned
  wg_pubkey     TEXT,
  public_ip     TEXT,
  cpu_free      INTEGER,
  mem_free_mib  INTEGER,
  last_seen     INTEGER
);

CREATE TABLE IF NOT EXISTS machines (   -- writer: host_id only
  id               TEXT NOT NULL PRIMARY KEY,
  name             TEXT,
  host_id          TEXT,
  state            TEXT,    -- creating|running|suspended|stopped|error|destroyed
  kind_knobs       TEXT,    -- json: auto_stop/auto_start/min_machines_running/soft_limit
  image_ref        TEXT,
  vcpus            INTEGER,
  mem_mib          INTEGER,
  domain           TEXT,
  custom_domain    TEXT,
  app_port         INTEGER,
  agent_port       INTEGER,
  agent_token_hash TEXT,
  mem_build_id     TEXT,    -- latest snapshot
  rootfs_build_id  TEXT,
  -- The template this machine was built from, pinned at create.
  --
  -- A machine's images are diffs whose unchanged ranges resolve against their
  -- parent, so the parent has to be the template it was actually diffed
  -- against -- not whichever template the host restoring it happens to hold.
  -- Those differ whenever the golden template is rebuilt, or a host mints its
  -- own, and the machine then cannot be restored there at all.
  template_mem_build_id    TEXT,
  template_rootfs_build_id TEXT,
  volume_id        TEXT,
  service_id       TEXT,
  release_id       TEXT,
  -- The app this machine belongs to. Grouping ONLY: it scopes .internal
  -- resolution and the tenant filter, and nothing reads it as a foreign key,
  -- because there is no apps table to point at.
  app              TEXT,
  -- The netns slot this machine holds on host_id, which is the low 16 bits of
  -- its mesh address.
  --
  -- Published rather than derived here because it is host-local knowledge:
  -- only the owner knows which slot it handed out. Every host then derives the
  -- ADDRESS from the owner's public key plus this index, exactly as it derives
  -- the owner's own address -- the address itself is never published, for the
  -- same reason hosts.wg_addr is never trusted.
  --
  -- It moves when a machine is rescued, which is why .internal answers carry a
  -- near-zero TTL.
  slot             INTEGER,
  last_activity    INTEGER,
  updated_at       INTEGER
);

CREATE TABLE IF NOT EXISTS checkpoints (
  id              TEXT NOT NULL PRIMARY KEY,
  machine_id      TEXT,
  seq             INTEGER,
  comment         TEXT,
  source_id       TEXT,
  mem_build_id    TEXT,
  rootfs_build_id TEXT,
  durable         INTEGER,
  created_at      INTEGER
);

CREATE TABLE IF NOT EXISTS api_keys (   -- writer: the dashboard's host
  hash       TEXT NOT NULL PRIMARY KEY,
  org_id     TEXT,
  scopes     TEXT,
  created_at INTEGER
);

-- The golden template every machine is created from.
--
-- FLEET-WIDE, not per host. A machine's memory image is a diff against the
-- template it was created from, so a host that built its own would be unable
-- to restore anyone else's machines -- which is the whole of cross-host
-- rescue. A host that has never built one reads this row and pulls the builds
-- it names from object storage.
-- The golden template's parts live in ONE column, not three.
--
-- Merging is per column. Three columns are three independent last-write-wins
-- races, so two hosts publishing at once could leave a row pairing one host's
-- memory build with the other's disk build -- a template that never existed,
-- pointing at a vmstate belonging to neither. Restores against it resolve
-- unchanged pages from the wrong parent, which is silent guest-memory
-- corruption, fleet-wide, with nothing to notice it.
--
-- One column cannot be merged into a value nobody wrote.
CREATE TABLE IF NOT EXISTS templates (
  id          TEXT NOT NULL PRIMARY KEY,   -- "golden"
  descriptor  TEXT,                        -- json: {mem_build_id, rootfs_build_id, snap_key}
  created_at  INTEGER
);

-- One deployable version of a service.
--
-- mem_build_id is what makes a deploy fast. The first replica of a release
-- boots from rootfs_build_id, passes its health gate, and is checkpointed --
-- and the resulting memory build is stamped here. Every later replica of the
-- same release, every scale-up, every rollback and every rescue then RESTORES
-- from that pair instead of booting, which is the difference between the
-- measured sub-second restore path and a cold boot nobody has budgeted.
--
-- Adding this column to a table that already carried rows would be the
-- fleet-killer: corrosion reads schema_paths at startup and cr-sqlite
-- backfills every existing row on a column add, which took fly's fleet down
-- twice for ~11.5h (fly.io/infra-log/2024-11-30). It is safe here and only
-- here because nothing has ever written this table -- zero rows, so the
-- backfill is a no-op. Once releases carries data, that door is closed: a
-- later shape change is a new table plus a dual-read migration.
CREATE TABLE IF NOT EXISTS releases (
  id              TEXT NOT NULL PRIMARY KEY,
  service_id      TEXT,
  rootfs_build_id TEXT,
  mem_build_id    TEXT,
  healthy         INTEGER,
  created_at      INTEGER
);

-- A persistent volume: one JuiceFS filesystem whose data lives in the same
-- bucket as everything else, holding one raw ext4 image that a machine gets
-- as a second virtio-blk drive.
--
-- host_id is the writer AND the lock, and that is not a policy choice. The
-- JuiceFS metadata engine is a single SQLite file replicated by Litestream,
-- and two hosts mounting one of those corrupts it -- so a volume is mounted by
-- exactly one host at a time, the host running its machine. The row moves only
-- when the machine moves, and a rescuing host takes it the same way it takes
-- the machine: by claiming it from an owner that has provably stopped
-- heartbeating.
--
-- There is no size column in mebibytes for decoration: size_mib is the size of
-- disk.img, fixed at create, because the guest formats it once and every later
-- boot expects the same block count.
CREATE TABLE IF NOT EXISTS volumes (    -- writer: host_id only
  id         TEXT NOT NULL PRIMARY KEY,
  name       TEXT,
  machine_id TEXT,
  size_mib   INTEGER,
  s3_prefix  TEXT,     -- volumes/<id>/ in the bucket; data and meta both
  mount_path TEXT,     -- where the guest mounts /dev/vdb
  host_id    TEXT,
  created_at INTEGER
);

-- A service is a set of machines that share a name, a release and an
-- environment. In this phase only its environment is consumed; the rollout
-- columns are here because a schema change is a fleet-wide re-bootstrap
-- (corrosion does not replicate DDL), so the shape lands once.
--
-- env holds non-secret values as json. env_sealed holds the secret ones as a
-- single sealed blob, and it is the ONLY form a secret may take here:
-- corrosion replicates every row to every host, so a plaintext value written
-- to one gossips to all of them and lands in every backup. The reference
-- implementation gets this wrong instructively -- uncloud stores each
-- container as json embedding its resolved env, fleet-wide.
--
-- Sealing happens in HOSTD, never in the client. A client that sealed would
-- need the fleet key, and the key would stop being fleet infrastructure the
-- moment it was handed to every laptop.
--
-- Health is a tagged union because a database image ships a command check and
-- not an HTTP one:
--   {"type":"http","path":...,"interval":...,"timeout":...,"grace":...,
--    "healthy_threshold":...}
--   {"type":"cmd","test":["CMD-SHELL","pg_isready -U postgres"],
--    "interval":...,"timeout":...,"grace":...,"retries":...}
-- Docker semantics, so every stock image's own HEALTHCHECK maps straight in.
CREATE TABLE IF NOT EXISTS services (  -- writer: host_id of the owning machines
  id            TEXT NOT NULL PRIMARY KEY,
  name          TEXT,
  app           TEXT,
  release_id    TEXT,
  replicas      INTEGER,
  health        TEXT,    -- json, tagged union (above)
  env           TEXT,    -- json, non-secret only
  env_sealed    TEXT,    -- sealed blob; never plaintext
  domain        TEXT,
  custom_domain TEXT,
  repo          TEXT,
  branch        TEXT,
  autodeploy    INTEGER,
  created_at    INTEGER);
