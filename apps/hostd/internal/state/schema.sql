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

CREATE TABLE IF NOT EXISTS api_keys (   -- writer: any host, on an admin-scoped request
  hash       TEXT NOT NULL PRIMARY KEY,
  org_id     TEXT,
  scopes     TEXT,
  created_at INTEGER
);

-- The golden template every machine is created from.
--
-- FLEET-WIDE within a vendor pool, not per host: the row id is
-- `golden-<vendor>`. A machine's memory image is a diff against the
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
  id          TEXT NOT NULL PRIMARY KEY,   -- "golden-<vendor>"
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
-- A custom hostname pointed at a service.
--
-- A NEW table rather than a column on services, and that is deliberate: this
-- schema is loaded by corrosion at agent start, and cr-sqlite backfills every
-- row of a table whose columns change. Adding one to services -- which already
-- carries rows -- is the fleet-wide gossip storm that took fly's fleet down
-- twice for ~11.5h. A new table backfills nothing because it has no rows.
--
-- The router matches on hostname at SNI time from its local replica, so the
-- lookup is a map hit rather than a query, and certmagic's on-demand decision
-- reads the same rows: a name that is not here gets no certificate, which is
-- what stops anyone who points DNS at our IPs from minting certs on our rate
-- limit.
CREATE TABLE IF NOT EXISTS domains (
  hostname   TEXT NOT NULL PRIMARY KEY,
  service_id TEXT,
  -- verified_at is 0 until the CNAME has been observed pointing at us. An
  -- unverified row never gets a certificate: issuance would burn rate limit
  -- on a name whose owner has not proved they want it here.
  verified_at INTEGER,
  created_at  INTEGER
);

-- The volume a service mounts, one row per replica's volume.
--
-- A NEW table rather than a column on services, for the reason domains is
-- one: services carries rows, and cr-sqlite backfills every row of a table
-- whose columns change. The key is <service_id>/<ordinal>. Today a service
-- mounts one volume and the API refuses more than one replica with it (a
-- volume is mounted by exactly one machine; see volumes below); a later
-- volume-per-replica shape is more rows here, never a column add.
--
-- Written once, on service create or promote, by the service's arbiter, the
-- host that writes the service row, BEFORE that row for the reason tenancy is
-- written before its object. Deleted with the service row.
CREATE TABLE IF NOT EXISTS service_volumes (  -- writer: the service's arbiter (write-once)
  id         TEXT NOT NULL PRIMARY KEY,   -- <service_id>/<ordinal>
  service_id TEXT,
  ordinal    INTEGER,                     -- 1 today; see above
  volume_id  TEXT,
  created_at INTEGER
);

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


-- Which org owns an object.
--
-- A NEW table, not an org_id column on machines, services and volumes: those
-- tables carry rows, and cr-sqlite backfills every row of a table whose
-- columns change. That is the fleet-wide gossip storm that took fly's fleet
-- down twice for ~11.5h (fly.io/infra-log/2024-11-30). A new table backfills
-- nothing because it has no rows.
--
-- The row is written BEFORE the object row it names, and never changed after.
-- That is what makes "any host" a legal writer here, where everything else in
-- this schema names one: two writers cannot disagree about a value written
-- once, so there is nothing for a last-write-wins merge to corrupt. Both
-- drivers write it ON CONFLICT DO NOTHING, so a second writer cannot change a
-- value even by accident.
--
-- Written first, and not after, so a create that dies partway leaves a tenancy
-- row naming an object that never appeared -- harmless, invisible, collected
-- with nothing -- rather than an object no org owns, which would be visible to
-- admin alone and to the tenant who created it not at all.
-- A build carries TWO rows: its job id (bld-...) scopes the log route, and
-- the rootfs build id it produces is what a deploy or a create names.
CREATE TABLE IF NOT EXISTS tenancy (   -- writer: the host writing the object row (write-once)
  id         TEXT NOT NULL PRIMARY KEY, -- machine, service, volume or build id
  org_id     TEXT,
  kind       TEXT,                      -- machine|service|volume|build
  created_at INTEGER
);

-- A revoked key.
--
-- A tombstone that only ever APPEARS, for the same reason a destroyed machine
-- is a state rather than a DELETE (see the top of this file): a delete racing
-- a replica that still carries the older insert loses the race through the
-- merge, and the row comes back. For a machine that is a resurrected sandbox;
-- for a key it is a revoked credential that authenticates again.
CREATE TABLE IF NOT EXISTS api_key_revocations (  -- writer: any host, on an admin-scoped request (write-once)
  hash       TEXT NOT NULL PRIMARY KEY,
  revoked_at INTEGER
);

-- What a RESTRICTED key may do, beyond its scopes.
--
-- A key handed to a coding agent through the OAuth consent screen is not the
-- same thing as the key an operator keeps: the human approving it chose "this
-- agent, these machines, this long". These three limits are what turn that
-- choice into something the fleet enforces rather than something the
-- dashboard merely remembers.
--
-- A NEW table rather than columns on api_keys, and that is not a style
-- preference: api_keys carries rows, and cr-sqlite backfills every row of a
-- table whose columns change, which is the gossip storm that took fly's fleet
-- down twice for ~11.5h. A new table backfills nothing.
--
-- WRITE-ONCE, keyed by the same hash as the key itself: the limits are chosen
-- at the moment the key is minted and can never be widened afterwards. That
-- is what lets any host write the row without a merge being able to corrupt
-- it, and it is also the security property -- a restriction that could be
-- edited later is not a restriction.
--
-- A key with NO row here is unrestricted, which is every key minted before
-- this table existed and every key an operator mints from the tokens page.
-- Reading it is therefore a miss for almost every request, which is why it
-- rides the same subscription cache the revocation check does.
CREATE TABLE IF NOT EXISTS api_key_limits (      -- writer: any host, on an admin-scoped request (write-once)
  hash         TEXT NOT NULL PRIMARY KEY,
  -- Every machine and service this key names must start with this. Empty
  -- means no naming restriction.
  name_prefix  TEXT,
  -- How many machines carrying that prefix may exist at once. 0 means no cap.
  -- Counted from the org's own rows at create time rather than tracked as a
  -- running total, because a count that has to be maintained is a count that
  -- drifts, and the rows are already local.
  max_machines INTEGER,
  -- Unix seconds after which the key authenticates nothing. 0 means it lives
  -- until it is revoked, which is what every operator key does.
  expires_at   INTEGER,
  created_at   INTEGER
);

-- Per-org limits.
--
-- One logical writer -- an admin request -- so unlike the two tables above
-- this one is updated in place, and last-write-wins between two admins
-- editing the same org is the intended semantics rather than a hazard.
CREATE TABLE IF NOT EXISTS org_quotas (          -- writer: any host, on an admin-scoped request
  org_id         TEXT NOT NULL PRIMARY KEY,
  max_machines   INTEGER,
  max_vcpus      INTEGER,
  max_mem_mib    INTEGER,
  max_volume_gib INTEGER,
  max_builds     INTEGER,
  updated_at     INTEGER
);

-- How much object storage an org's checkpoints may hold.
--
-- A SEPARATE table rather than a column on org_quotas, and that is not a
-- style choice: org_quotas carries rows on every running fleet, and cr-sqlite
-- backfills every row of a table whose columns change and gossips the
-- backfill. That is the fleet-wide storm that took fly down twice for ~11.5h
-- (rule 6). A new table backfills nothing because it has no rows.
--
-- Read alongside org_quotas by GetQuota, so a caller sees one quota; an org
-- with no row here has the default, exactly as an org with no org_quotas row
-- does.
CREATE TABLE IF NOT EXISTS org_snapshot_quotas (  -- writer: any host, on an admin-scoped request
  org_id           TEXT NOT NULL PRIMARY KEY,
  max_snapshot_gib INTEGER,
  updated_at       INTEGER
);

-- Which repositories an org may ask this fleet to fetch.
--
-- The fleet's GitHub App holds an installation token for every repository it
-- is installed on. Without a row saying otherwise, `POST /v1/builds` and
-- `POST /v1/plan` with a {repo, ref} body would let ANY key name ANY of those
-- repositories, build it, and boot a shell inside another tenant's source
-- under the fleet's own credential. This table is the answer to "may this org
-- fetch this repository?", and hostd answers it from its LOCAL replica --
-- never from the dashboard's database, which the data plane may not depend on
-- (ARCHITECTURE.md invariant 2).
--
-- A NEW table, and not a column on services, for two independent reasons.
-- The mechanical one: services carries rows, and cr-sqlite backfills every row
-- of a table whose columns change, which is the fleet-wide gossip storm that
-- took fly's fleet down twice for ~11.5h. The authorization one: services.repo
-- is written by whoever creates a service, so a permission read out of it
-- would be granted by the very caller it is meant to constrain.
--
-- WRITE-ONCE, exactly like tenancy, and that is what makes "any host may write
-- it" safe where nearly every other table here names one writer: the row's
-- whole content IS its key, so two hosts racing cannot merge into a value
-- neither wrote, and both drivers write ON CONFLICT DO NOTHING so a second
-- writer cannot change one even by accident.
--
-- The key is <org_id>/<repo> and NOT <repo>: one repository may legitimately
-- be connected to more than one org (a public repo two tenants both deploy),
-- and a repo-keyed row would turn the first claim into a fleet-wide land grab
-- on that name -- a tenant could park on `acme/shop` and lock its real owner
-- out. Lowercased on the way in, because GitHub owner and repository names are
-- case-insensitive and `Acme/Shop` must not read back as unconnected.
--
-- WHO may write one is the API's question, not the schema's: connecting is
-- admin-scoped (POST /v1/repos), because the proof that an org controls a
-- repository is held at GitHub -- the App installation, bound to an org by the
-- dashboard's install callback -- and hostd cannot check it from a request.
-- FETCHING is not admin-scoped, and that is the whole point of the table: a
-- tenant-scoped key deploys from a repository its own org is connected to.
--
-- There is no disconnect yet, deliberately. Removing a link is a
-- tombstone-shaped problem -- a DELETE loses to a replica still carrying the
-- insert and the link comes back, see api_key_revocations -- so it wants its
-- own write-once table and its own decision. Until then access is cut where it
-- is granted: uninstall the App from the repository, or revoke the org's keys.
CREATE TABLE IF NOT EXISTS repo_links (          -- writer: any host, on an admin-scoped request (write-once)
  id           TEXT NOT NULL PRIMARY KEY,        -- <org_id>/<owner>/<name>, lowercased
  org_id       TEXT,
  repo         TEXT,                             -- owner/name, lowercased
  connected_at INTEGER
);

-- Which CPU vendor a host is, and which vendor photographed a memory image.
--
-- Two NEW tables rather than a column on hosts or machines: both carry rows,
-- and cr-sqlite backfills every row of a table whose columns change (the
-- storm that took fly down twice for ~11.5h). A new table backfills nothing.
-- A memory snapshot never restores across the Intel/AMD boundary; the rescue
-- hash filters survivors by host_cpu, and a host of the other vendor boots
-- the machine from its disk instead (ARCHITECTURE.md rule 6).
CREATE TABLE IF NOT EXISTS host_cpu (          -- writer: the host itself
  host_id      TEXT NOT NULL PRIMARY KEY,
  vendor       TEXT,     -- /proc/cpuinfo vendor_id: GenuineIntel | AuthenticAMD
  cpu_template TEXT,     -- PILOT_CPU_TEMPLATE; empty on a dev host
  updated_at   INTEGER
);

-- What a host can still hold, for create-time placement.
--
-- A side table rather than columns on `hosts` for the reason host_cpu is one:
-- `hosts` has rows, and a column add on a live cr-sqlite table backfills and
-- gossips every one of them (rule 6).
--
-- mem_reclaimable_mib is the memory held by RUNNING machines this host would
-- suspend anyway, were the idle timer to fire now. It is NOT suspended
-- machines: suspend kills the Firecracker process, so a suspended machine
-- already holds no memory. Counting it would double-count free memory and
-- admit creates that then fail to boot.
--
-- draining is set by `pilot hosts drain`. A draining host is skipped by every
-- ranker, which is what makes a drain converge rather than race the placer.
--
-- Writer: the host itself, on its heartbeat.
CREATE TABLE IF NOT EXISTS host_capacity (     -- writer: the host itself
  host_id             TEXT NOT NULL PRIMARY KEY,
  mem_free_mib        INTEGER,
  mem_reclaimable_mib INTEGER,
  cpu_count           INTEGER,
  vcpus_running       INTEGER,
  draining            INTEGER,  -- 1 while the host is being drained
  updated_at          INTEGER
);

-- Which builds a host already has on local disk.
--
-- Placement prefers a host that holds the builds a create needs, because a
-- cached build is the difference between a restore and a download. A BONUS
-- only: it breaks a near-tie and can never move a machine onto a host that
-- cannot hold it, or the fleet would pack itself onto whichever host happened
-- to build things.
--
-- Capped at the newest few hundred ids, deliberately. An unbounded row here is
-- the C5 landmine: a large value gossiped on every change starves the apply
-- loop for every other row.
--
-- Writer: the host itself, and only when the set actually changed.
CREATE TABLE IF NOT EXISTS host_builds (       -- writer: the host itself
  host_id    TEXT NOT NULL PRIMARY KEY,
  builds     TEXT,     -- json array of build ids present on this host
  updated_at INTEGER
);

-- One host OFFERING a machine to another, on a planned drain.
--
-- This is the third sanctioned exception to single-writer, and the only one
-- where a LIVE host's machine changes owner. It exists because the alternative
-- is worse: without it, a host reboot is customer-visible, since a machine only
-- ever moved when its owner was provably dead.
--
-- Why it is safe where an ordinary cross-host write is not:
--
--   * WRITE-ONCE. A handoff row is inserted and never updated. A CRDT merge
--     has nothing to corrupt in a row nobody rewrites.
--   * The SOURCE writes it, and the source is the machine's current owner, so
--     the row is written by the host that already owns what it describes.
--   * The target's claim is checked against it: to_host must be the claimer,
--     from_host must be the row's current owner, it must be the machine's
--     newest offer, and the machine must not be running. A claim that fails
--     any of those is refused exactly as a claim with no dead owner is.
--
-- seq orders repeated offers of one machine: a target that never took it is
-- superseded by the next offer rather than racing it.
--
-- Reaped by their writer after a day, like destroyed machines.
-- How often a volume is snapshotted, and how many snapshots are kept.
--
-- A scheduled snapshot is the difference between "you can roll back" and "you
-- can roll back to a moment you thought to record". Nobody takes a manual
-- snapshot before the mistake.
--
-- Retention is two numbers rather than one because the two questions are
-- different: how far back can I go at a day's resolution, and how far back can
-- I go at all. Keeping the newest N dailies plus the newest of each of M weeks
-- answers both in bounded space.
--
-- A NEW table rather than columns on `volumes`, which has rows (rule 6).
--
-- Writer: the volume's host, which is the only host that can take the snapshot
-- the policy describes.
CREATE TABLE IF NOT EXISTS volume_policies (   -- writer: the volume's host_id
  volume_id   TEXT NOT NULL PRIMARY KEY,
  cron        TEXT,     -- five fields UTC, or @daily/@weekly/@hourly/@monthly
  keep_daily  INTEGER,  -- the newest N snapshots
  keep_weekly INTEGER,  -- plus the newest of each of the last M ISO weeks
  updated_at  INTEGER
);

-- Where a forked machine came from.
--
-- A fork is a NEW machine restored from another machine's memory and disk: new
-- id, new name, new URL, new token. What it shares with its parent is the
-- artifacts it was restored from, and that sharing is the whole reason this
-- table exists -- without a record of it, the parent's next suspend or destroy
-- would discard builds the fork is still faulting pages out of.
--
-- A side table rather than a `parent` column on machines, because `machines`
-- has rows (rule 6).
--
-- Write-once, by the fork's own host, which is the host that created the fork
-- and therefore already writes its machine row.
CREATE TABLE IF NOT EXISTS machine_lineage (   -- writer: the fork's host (write-once)
  id              TEXT NOT NULL PRIMARY KEY,   -- the FORK's machine id
  parent_id       TEXT,     -- the machine it came from, "" for a checkpoint with no live parent
  checkpoint_id   TEXT,     -- the checkpoint it was restored from, "" for a suspend-image fork
  mem_build_id    TEXT,     -- the artifacts it shares with its parent, and
  rootfs_build_id TEXT,     -- which therefore must outlive the parent
  volume_snapshot TEXT,     -- the volume snapshot its own volume was filled from
  created_at      INTEGER
);

CREATE TABLE IF NOT EXISTS machine_handoffs (  -- writer: the machine's owner (write-once)
  id         TEXT NOT NULL PRIMARY KEY,        -- ho-<uuid>
  machine_id TEXT,
  from_host  TEXT,
  to_host    TEXT,
  seq        INTEGER,
  created_at INTEGER
);

-- Keyed like tenancy: the id of the object whose memory image this describes.
-- A release's and a checkpoint's images are as vendor-locked as a machine's.
-- last_start and last_start_at are written for machines only: the observable
-- record of a resume that was downgraded to a cold boot.
CREATE TABLE IF NOT EXISTS machine_cpu (       -- writer: the host that writes the object row it describes
  id            TEXT NOT NULL PRIMARY KEY,     -- machine, release or checkpoint id
  kind          TEXT,     -- machine|release|checkpoint
  vendor        TEXT,
  last_start    TEXT,     -- restore|boot|cold_boot
  last_start_at INTEGER,
  updated_at    INTEGER
);

-- Labels a caller attached at create, for finding an object again: an agent
-- running twenty sandboxes for one task has only name prefixes otherwise.
-- Keyed like machine_cpu: the id of the machine or service the labels
-- describe, written by the host that writes that object's row, and written
-- ONCE at create so the CRDT merge has nothing to corrupt. A side table
-- rather than a column on machines or services, which have rows (rule 6).
CREATE TABLE IF NOT EXISTS machine_labels (    -- writer: the host that writes the object row it describes (write-once)
  id         TEXT NOT NULL PRIMARY KEY,        -- machine or service id
  kind       TEXT,     -- machine|service
  labels     TEXT,     -- json object, string -> string
  updated_at INTEGER
);

-- Who may reach an object's URL. Absent means public, which is what every
-- URL was before this table existed; `org` means the router asks for an API
-- key of the owning org. Keyed and written like machine_labels: the object's
-- id, by the host that writes its row, so the merge has one writer.
CREATE TABLE IF NOT EXISTS url_auth (          -- writer: the host that writes the object row it describes
  id         TEXT NOT NULL PRIMARY KEY,        -- machine or service id
  kind       TEXT,     -- machine|service
  mode       TEXT,     -- public|org
  updated_at INTEGER
);

-- What a machine may ask its host's broker for.
--
-- A machine holds no API key. It asks the broker hostd binds inside its own
-- network namespace, and this row says what the answer may be: which scopes a
-- token may carry, and which secret values may be handed over. Absent, or
-- present with both fields empty, means NO -- deny by default, so a machine
-- that nobody granted anything to can reach nothing.
--
-- `sealed` is a seal.Seal of a json name-to-value map, never plaintext: this
-- table gossips to every host like every other one. These are the secrets that
-- deliberately never reach /etc/pilot/env, so they are in no snapshot and on no
-- disk inside the guest.
--
-- Keyed and written like url_auth above: the object's id, written by the host
-- that writes its row, so the merge has one logical writer (invariant 1). A
-- side table rather than columns on `machines`, because that table has rows and
-- is therefore closed to column adds (rule 6).
CREATE TABLE IF NOT EXISTS broker_grants (     -- writer: the host that writes the object row it describes
  id         TEXT NOT NULL PRIMARY KEY,        -- machine or service id
  kind       TEXT,     -- machine|service
  org_id     TEXT,
  scopes     TEXT,     -- csv of api scopes; empty means no token may be minted
  sealed     TEXT,     -- seal.Seal of {name: value}; empty means no secrets
  updated_at INTEGER
);

-- How big a service's replicas are. Absent means the defaults every service
-- had before this table existed (1 vCPU, 512 MiB), so an old service reads
-- correctly without being backfilled -- which matters because backfilling a
-- live cr-sqlite table is the incident rule 6 exists to prevent.
--
-- A side table rather than two columns on `services`, for that same reason:
-- `services` has rows.
--
-- image_vcpus and image_mem_mib are the size the release's MEMORY IMAGE was
-- photographed at, which is NOT always the current size: a resize changes the
-- size first and re-photographs after. A replica may only restore from that
-- image when the two agree, because Firecracker cannot load a memory image
-- into a differently-sized VM. When they disagree the replica boots instead,
-- which is slower and correct.
--
-- Writer: the service's arbiter, the one host that already writes the
-- `services` row through forwardToArbiter, so the merge has a single writer.
-- The IPv6 block a host hands per-org egress addresses out of.
--
-- A tenant's outbound address is a pure function of this prefix and their org
-- id, so there is nothing to allocate and no assignment to store. What DOES
-- have to be shared is the prefix itself: any host may answer a request about
-- any machine, and the answering host cannot know another host's prefix
-- without reading it.
--
-- Absent means that host manages no egress, which is what every host did
-- before this table, so nothing is backfilled. A side table rather than a
-- column on `hosts` for the usual reason: `hosts` has rows (rule 6).
--
-- Writer: the host the row describes, which is the plainest single writer
-- there is.
CREATE TABLE IF NOT EXISTS host_egress (       -- writer: the host it describes
  host_id    TEXT NOT NULL PRIMARY KEY,
  prefix6    TEXT,     -- a routed /64, e.g. 2a01:4f8:1c17:abcd::/64
  interface  TEXT,     -- the uplink it leaves by, for an operator reading this
  updated_at INTEGER
);

CREATE TABLE IF NOT EXISTS service_sizes (     -- writer: the service's arbiter
  service_id    TEXT NOT NULL PRIMARY KEY,
  vcpus         INTEGER,  -- what a replica is created with
  mem_mib       INTEGER,
  image_vcpus   INTEGER,  -- what the release's memory image was photographed at
  image_mem_mib INTEGER,
  updated_at    INTEGER
);
