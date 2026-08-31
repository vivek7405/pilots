-- pilots cluster state.
--
-- This schema is loaded verbatim by Corrosion from phase 4 onward, so it must
-- stay CRDT-safe: cr-sqlite merges rows last-write-wins across hosts and
-- CANNOT enforce constraints. Therefore, deliberately, there are
--   no UNIQUE, no NOT NULL, no FOREIGN KEY, no CHECK
-- anywhere below. Uniqueness (machine names, domains) comes from
-- deterministic ownership -- hash(key) mod live_hosts -- not from the database.
--
-- The `writer:` comments are the only in-code record of the single-writer
-- invariant: a host writes ONLY rows describing its own machines. Violating
-- it does not error, it corrupts state silently. See ARCHITECTURE.md.

CREATE TABLE IF NOT EXISTS hosts (      -- writer: the host itself
  id            TEXT PRIMARY KEY,
  wg_addr       TEXT,
  public_ip     TEXT,
  cpu_free      INTEGER,
  mem_free_mib  INTEGER,
  last_seen     INTEGER
);

CREATE TABLE IF NOT EXISTS machines (   -- writer: host_id only
  id               TEXT PRIMARY KEY,
  name             TEXT,
  host_id          TEXT,
  state            TEXT,    -- creating|running|suspended|stopped|error
  kind_knobs       TEXT,    -- json: autoStop/autoStart/minRunning/softLimit
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
  volume_id        TEXT,
  service_id       TEXT,
  release_id       TEXT,
  last_activity    INTEGER,
  updated_at       INTEGER
);

CREATE TABLE IF NOT EXISTS checkpoints (
  id              TEXT PRIMARY KEY,
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
  hash       TEXT PRIMARY KEY,
  org_id     TEXT,
  scopes     TEXT,
  created_at INTEGER
);

CREATE TABLE IF NOT EXISTS releases (
  id              TEXT PRIMARY KEY,
  service_id      TEXT,
  rootfs_build_id TEXT,
  healthy         INTEGER,
  created_at      INTEGER
);
