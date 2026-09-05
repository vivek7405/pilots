# Postgres on pilots

`pilot add postgres` writes a compose fragment. There is no
Postgres-as-a-service here and there is not meant to be: what the command adds
is a service, a volume and a password, all of them primitives the platform
already has. The one part worth a document is durability, because the two modes
trade different things and neither is strictly better.

## Why there are two modes

A pilots volume is a JuiceFS filesystem over S3, and it is durable per write.
That is the right default for application data and the wrong one for a Postgres
data directory: per-write durability means an object-storage round trip on every
`fsync`, and Postgres `fsync`s on every commit.

So the default puts the data directory on the machine's local disk and ships the
write-ahead log to a volume instead. Commits stay local and fast; the recovery
point is bounded by `archive_timeout` rather than zero.

| | default: local data dir + WAL archive | `--durable-volume` |
|---|---|---|
| Data directory | machine-local disk | pilots volume |
| Commit latency | local | one object-storage round trip |
| RPO | ≤ 60 s (`archive_timeout`) | 0 |
| Recovery | restore the base backup, replay WAL | mount the volume |
| Generated files | `.pilots/postgres/` | none |

**State which one is in use.** The fragment records it as
`x-pilots.durable_volume`, with the trade written on the same line, so a file
somebody opens in six months says which side it took.

Neither mode changes what the replica costs while it is up, but suspension
does: a suspended machine bills storage only -- wall-clock machine-seconds and
volume-GiB-seconds accrue, vCPU-seconds and MiB-seconds do not. A Postgres
service takes the ordinary floor of zero: an idle replica suspends, its volume
stays claimed and mounted, and the next connection over `postgres.internal`
wakes it, because the host counts guest-to-guest traffic and holds a replica
with an open session. A database that must stay resident sets
`x-pilots.min_machines_running: 1`, which travels on the deploy.

## The default: local data directory, WAL shipped to a volume

```yaml
services:
  postgres:
    build: ./.pilots/postgres
    command:
      - postgres
      - -c
      - archive_mode=on
      - -c
      - archive_timeout=60
      - -c
      - archive_command=test ! -f /archive/wal/%f && cp %p /archive/wal/%f
    environment:
      POSTGRES_PASSWORD: secret://postgres_password
      PGDATA: /var/lib/postgresql/data
    volumes:
      - pgarchive:/archive
    healthcheck:
      test: [ CMD-SHELL, pg_isready -U postgres ]
      interval: 10s
      timeout: 5s
      retries: 5
    x-pilots:
      durable_volume: false # data dir local, WAL shipped to the pgarchive volume every 60s (RPO 60s)
volumes:
  pgarchive: {}
```

The archive target is a **pilots volume**, not S3 directly. The stock
`postgres:17` image carries no S3 tooling and pilots has no bind mounts, but a
volume is JuiceFS over S3, so a plain `cp` into `/archive` lands in object
storage with nothing added to the image.

A service that mounts a volume runs one replica, and `pilot deploy` restarts
that one machine in place on a redeploy. Postgres is down from the moment the
old process is killed until the new one passes `pg_isready`; a request arriving
meanwhile waits rather than failing, and a peer's connection is refused until
then. `/archive` is untouched by the restart.

Two files are generated beside the compose file:

```dockerfile
# .pilots/postgres/Dockerfile
FROM postgres:17
COPY 10-base-backup.sh /docker-entrypoint-initdb.d/
```

```sh
# .pilots/postgres/10-base-backup.sh
set -e
mkdir -p /archive/wal /archive/base
pg_basebackup -U postgres -D /archive/base -Ft -z -X none
```

The base backup is taken **during initdb**, not afterwards. The entrypoint
starts its temporary server with the same `command` arguments, so archiving is
already on when the first WAL segment is written, and the backup is consistent
with the archive that follows it. Point-in-time recovery is this backup plus
every segment archived since.

### Refreshing the base backup

The archive grows without bound until a newer base backup lets you discard the
segments before it. Take one whenever the archive gets large, or on a schedule:

```
pilot machines exec postgres -- pg_basebackup -U postgres -D /archive/base -Ft -z -X none
```

### Recovering

1. Create a service on the **same** `pgarchive` volume.
2. Untar the base backup into a fresh data directory:
   ```
   mkdir -p /var/lib/postgresql/data
   tar -xzf /archive/base/base.tar.gz -C /var/lib/postgresql/data
   ```
3. Point recovery at the archive and mark the directory for recovery:
   ```
   echo "restore_command = 'cp /archive/wal/%f %p'" >> /var/lib/postgresql/data/postgresql.conf
   touch /var/lib/postgresql/data/recovery.signal
   ```
   To stop at a moment rather than at the end of the archive, add
   `recovery_target_time = '2026-09-04 11:00:00+00'`.
4. Start Postgres. It replays WAL and promotes itself when it reaches the target.

## `--durable-volume`: the data directory on a volume

```yaml
services:
  postgres:
    image: postgres:17
    environment:
      POSTGRES_PASSWORD: secret://postgres_password
      PGDATA: /var/lib/postgresql/data
    volumes:
      - pgdata:/var/lib/postgresql/data
    healthcheck:
      test: [ CMD-SHELL, pg_isready -U postgres ]
      interval: 10s
      timeout: 5s
      retries: 5
    x-pilots:
      durable_volume: true # data dir on a volume: RPO 0, at an object-storage round trip per commit
volumes:
  pgdata: {}
```

No generated files, no archive, no recovery procedure: the data directory is
already durable per write, and a host that dies remounts the volume wherever
the machine is rescued. The price is on every commit.

A service that mounts a volume runs one replica, and `pilot deploy` restarts
that one machine in place on a redeploy. Postgres is down from the moment the
old process is killed until the new one passes `pg_isready`; a request arriving
meanwhile waits rather than failing, and a peer's connection is refused until
then. The data directory is untouched by the restart.

Worth it for low-write, high-value data. Not worth it for anything with a busy
write path.

## The password and `DATABASE_URL`

`pilot add postgres` generates a 32-character password, prints it **once** to
stderr, and stores it in the credentials file under the app:

```json
{ "secrets": { "shop": {
    "postgres_password": "...",
    "database_url": "postgres://postgres:...@postgres.internal:5432/postgres"
} } }
```

`postgres.internal` is the service name as the `.internal` resolver serves it;
there is no app prefix in the name.

Point the services that need it at the reference rather than the value:

```yaml
services:
  web:
    environment:
      DATABASE_URL: secret://database_url
```

On another machine, or in CI, export the values instead:

```
export PILOT_SECRET_POSTGRES_PASSWORD=...
export PILOT_SECRET_DATABASE_URL=postgres://postgres:...@postgres.internal:5432/postgres
```

## One thing to carry into operations

**A rescued database has a different RTO from every other machine.** Everything
else on the platform restores from its snapshot instantly. A database in the
default mode restores, and *then* replays WAL. Size the expectation accordingly
when a host dies.
