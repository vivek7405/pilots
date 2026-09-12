# What we operate, and what you operate

This page exists because the most expensive thing a platform can do is look
like more than it is. Everything below is what you would find out anyway — the
question is only whether you find it out now or at three in the morning.

## The short version

**We operate the platform. You operate your database.**

A database on pilots is a machine running the stock image, with a volume and a
snapshot schedule. That is a genuinely good place to run one, and it is not the
same thing as a managed database. Nobody here is paged when your Postgres runs
out of connections, and nobody here notices before you do.

## What "managed" usually means, and which parts you get

| What a managed database gives you | Here |
|---|---|
| The right configuration, made for you | **Yes.** `pilot add postgres` writes it. |
| A volume that survives the host | **Yes.** Every write is in object storage. |
| Automatic backups, with retention | **Yes.** Daily by default, seven dailies and four weeklies kept. |
| Point-in-time recovery | **Yes**, for Postgres: a base backup plus archived write-ahead log. |
| Restore to a new database, not over the old one | **Yes.** A snapshot forks into a separate volume. |
| Somebody on call for it | **No.** That is you. |
| Version upgrades applied for you | **No.** You change the image tag and deploy. |
| Failover to a replica within seconds | **No.** See the recovery times below. |
| Tuning, index advice, query review | **No.** |

The first five are most of the practical value, and they are the ones a
platform can honestly deliver. The last four are a company, not a feature. Fly
built that company and then wrote about what it cost; we decided not to, and
the whole of this page is the consequence of that decision being visible rather
than hidden.

## How much data you can lose, exactly

This is the number nobody publishes, so here it is.

**Postgres in the default `wal-archive` mode: up to 60 seconds.**

The data directory is on the machine's local disk. Write-ahead log segments are
shipped to the volume when they fill or when `archive_timeout` expires, which
is sixty seconds. If the host dies, everything committed since the last shipped
segment is gone. Not corrupted — gone, and not recoverable from anywhere,
because it only ever existed on that host's disk.

In exchange, a commit is a local disk write. It does not wait for object
storage.

**Postgres in `durable-volume` mode, and every other engine: zero.**

The data directory is on the volume, and a volume write is in object storage
before it is acknowledged. Nothing committed is lost, whatever happens to the
host.

In exchange, every commit waits for an object-storage round trip. For a
write-heavy workload that is the difference between a fast database and a slow
one, which is exactly why it is not the default.

`pilot add` prints which of these you chose, every time, because choosing it
silently would make this page a formality.

## How long recovery takes, and why a database is different

Every other machine on this platform comes back in under a second. It is
restored from a memory image: the processes are already running, the memory is
already warm, and the wake is a page-fault-on-demand resume.

**A database is not like that**, and it is the one place the platform's own
headline number does not apply.

- **A host dies, wal-archive mode.** The machine is rescued onto another host
  and cold-boots, because its memory image describes a filesystem state that
  the archived WAL has moved past. Postgres then replays the archive. Recovery
  is a boot plus a replay, and the replay is proportional to how much was
  written since the last base backup. Seconds to minutes.
- **A host dies, durable-volume mode.** The same rescue, then a volume mount,
  then Postgres's own crash recovery. Seconds.
- **You restore a snapshot.** The machine cold-boots, deliberately: its memory
  image cached the filesystem you just replaced, and waking it onto the restored
  disk would corrupt the new one within seconds. Seconds to a minute.
- **Point-in-time recovery.** A base backup is untarred and the WAL is replayed
  to the moment you named. Minutes, and proportional to the distance from the
  base backup.

Every one of those is bounded and none of them is instant. If your application
cannot tolerate minutes of database downtime, you need a replica, and you need
to be the one who decides what failover means for your data.

## What is NOT automatic

- **Nothing fails over.** A single database machine is a single point of
  failure for whatever depends on it. The machine will be rescued onto another
  host when its host dies, which is not the same as a standby taking over: the
  database is down for the length of the recovery above.
- **Nothing upgrades itself.** `postgres:17` stays `postgres:17` until you
  change it. That is deliberate — a database engine upgrading itself under a
  running application is not a feature anyone wants — but it means the upgrade
  is a thing you have to do.
- **Nothing tunes itself.** The recipe sets what is needed for durability and
  nothing else. `shared_buffers`, connection limits and indexes are yours.
- **Nothing watches your query patterns.** `pilot metrics` shows what the
  engine reports about itself. Reading it is you.

## What IS automatic

- **Snapshots**, daily by default, with retention that keeps seven dailies and
  the newest of each of four weeks.
- **The volume**, which is in object storage on every write and survives losing
  the host's disk entirely.
- **A filesystem check** before any guest is given a volume, because a host
  that died mid-write leaves an image that still says it is clean.
- **Rescue**, when a host stops heartbeating: the machine comes back on another
  host with the same id, name and URL.
- **The health gate**, so a deploy that cannot reach the database does not
  become the running release.

## High availability, and whose it is

`pilot db ha enable` runs several Postgres machines with automatic failover, on
Patroni, with its own etcd beside them. Read this before turning it on, because
the failover is the part that makes people assume somebody is on call.

**What pilots operates.** The machines, the volumes, the snapshots, the process
supervisor, and the placement of each node on a different host where the fleet
has one. When a host dies, its node comes back on another host with the same
name and the same volume.

**What pilots does not operate.** Patroni's choice of leader. The replication
tuning. The decision to fail back. The three in the morning. Nothing in the
platform reads or writes which node is primary, and that is deliberate: the
proxy in front of the database follows Patroni's own health endpoint, checked
every second, because a leader recorded in a replicated row would be stale
exactly when it mattered.

**What it costs.** Two Postgres machines and three etcd members is the smallest
sensible cluster, which is five machines, each with its own volume, all of them
billed. etcd must be an odd number: an even one has no majority it did not
already have at one fewer member, so it buys failure modes and no availability.

**What a failover looks like from outside.** Your connection string does not
change, and the address does not change. A connection that was open to the old
primary is reset, which is physics rather than a policy: the process it was
talking to is gone. A new connection reaches the new primary once the proxy's
next check notices, which is seconds rather than minutes.

**What it does not protect you from.** A bad migration, a wrong DELETE, or
anything else that replicates faithfully to every node. That is what
`pilot db restore` is for, and it is a different tool for a different failure.

## If you need more than this

You need a managed database, and you should use one. Point `DATABASE_URL` at
it. The platform does not care where your database is, and an application here
talking to a database elsewhere is an ordinary configuration rather than a
workaround.

That is not a defeat. It is the same decision we made about building the tier:
the honest answer to "should we operate your database" is usually no, and a
platform that says so is more useful than one that implies otherwise.
