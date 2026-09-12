# Services

## What this covers

A durable thing behind a permanent URL: finding one, reading its releases, rolling back, reading a replica's logs, and what the health gate is doing. Not covered: getting the first URL (deploy.md), why a deploy failed (errors.md).

## The command to copy

    pilot services ls
    pilot services info web

MCP: `list_services`, then `service` with `{ "service": "web" }`.

## What happens

A service is one or more machines behind a URL that never changes. That URL is `<name>.<fleet domain>`, minted when the service is created, and a request to it reaches whatever machines the current release has, so a deploy replaces every replica without moving the address. A service that has nothing to serve on 8080, a database for instance, asks for no address with `private: true` on the API or `x-pilots.private: true` in a compose file; peers still reach it at `<name>.internal`.

A deploy builds a rootfs, creates a release, starts a replica from it, and lets the release take traffic only once the replica has passed its health check. That gate is why a broken deploy does not take the previous one down.

## Scheduled jobs

A cron is a request on a schedule. Declare it and the platform GETs the path at the right minute, waking the machine if it is asleep; the machine sleeps again afterwards, so a job that runs for a minute a day costs a minute a day.

    x-pilots:
      schedules:
        - cron: "0 5 * * *"      # five fields, UTC; or @hourly, @daily, @weekly, @monthly
          path: /jobs/digest
        - cron: "@hourly"
          cmd: /app/bin/tick      # a command instead, for work with no route; it runs as the app user from its home, so spell the path out

An app can declare them in its own config instead, and `deploy` reads it, so a cron needs no pilots-specific file at all:

| The app has | Write |
| --- | --- |
| a `vercel.json` (Next, Astro, SvelteKit, Nuxt, Remix — anything) | `{ "crons": [{ "path": "/api/digest", "schedule": "0 5 * * *" }] }` |
| a webjs `package.json` | `"webjs": { "crons": [{ "path": "/jobs/digest", "schedule": "0 5 * * *" }] }` |

Same shape either way, and `vercel.json` is read whatever the app is written in — a Rails or Django service with that file gets its crons too. A webjs app's own block wins where both are present. A sandbox takes `pilot machines create --schedule "0 5 * * * GET /jobs/digest"` (or `--schedule "@hourly /usr/local/bin/backup.sh"` for a command).

A `path` job is an ordinary request, so it needs a machine that can wake: `auto_start: false` together with the default `auto_stop: suspend` is refused at create, because the job could never run. A `cmd` job is not a request and takes neither.

What the handler sees is a `GET` carrying `X-Pilot-Cron: <expression>`. The public edge strips that header from every outside request, so `if (!req.headers['x-pilot-cron']) return 403` is the whole check, with no secret to keep. A job can fire twice in rare cases (a host restart or a deploy inside its minute), so make it idempotent; a job still running when its next minute comes is skipped, not overlapped. One replica fires for a service, however many it has. To remove every cron, deploy with `schedules: []` (or `crons: []`); an absent key keeps the previous release's.

A replica with no traffic suspends after about 30 seconds of quiet and the next request wakes it; that is the default and it costs nothing while asleep. A worker that must keep running with nothing connected to it -- a queue consumer, a scheduler -- keeps one replica resident with `x-pilots: min_machines_running: 1` in its compose entry (or `auto_stop: off`). The scale-down window is the autoscaler's and is not a knob; `idle_timeout` is the sandbox timer (sandboxes.md). The keys are listed in compose.md.

## Databases

A database here is an ordinary service. `pilot add postgres` (also `mysql`, `redis`, `mongo`) writes a compose entry, a volume, a snapshot policy and a generated password into the project. Nothing about it is a second system: the same rollout, the same health gate, the same volume, the same snapshots. Nobody operates it for you, and `docs/honesty.md` says exactly which half is whose.

The recipe carries a `pilot.engine` label, which is what makes the rest of this table work.

| I need to... | Command | Note |
| --- | --- | --- |
| add one | `pilot add postgres` | writes the entry, the password and the durability statement; read that statement |
| a session on it | `pilot db connect` | the engine's own client, over a tunnel, or inside the machine if you have neither the client nor the password |
| what the engine says | `pilot metrics <service>` | connections against the limit, cache hits, commits against rollbacks |
| recover to a moment | `pilot db restore <service> --to <RFC3339>` | Postgres in wal-archive mode; runs as a NEW service beside the old one |
| a port held open | `pilot proxy` | for a tool that is not a shell |

### Two addresses, and which is which

A Postgres added with a pooler publishes two, and the difference is not cosmetic:

| Variable | Port | For |
| --- | --- | --- |
| `DATABASE_URL` | 6432 | the application. pgbouncer in transaction mode, so hundreds of client connections sit on a handful of server ones |
| `DATABASE_URL_DIRECT` | 5432 | migrations, `LISTEN`/`NOTIFY`, session advisory locks, temporary tables, and any `SET` meant to outlive a transaction |

Transaction pooling is not a superset of a direct connection. A migration tool pointed at the pooler fails in ways that read as a broken migration rather than as a wrong address, which is why both are set and named rather than one being left to be rediscovered.

### Durability, in one line each

| Mode | Loses at most | Costs |
| --- | --- | --- |
| `wal-archive` (default) | 60 seconds of writes | nothing per commit; segments ship to the volume every minute |
| `--durable-volume` | nothing | an object-storage round trip on every commit |

The default is the first, because sixty seconds of exposure on a machine that has not crashed beats putting object-storage latency in the commit path of every write. Choose the other one deliberately.

### Recovery

`pilot db restore` replays the write-ahead log onto the newest base backup at or before the moment you name, as a NEW private service on a FORK of the archive volume. The original keeps serving throughout, so you can query both and compare before pointing anything at either. `pg_isready` passes only after the recovery promotes, so the release health gate is the restore gate: a recovery that never reaches its target never becomes a running release.

How far back you can go is bounded by the oldest base backup the archive still holds, which is four weeks by default.

## The tools

| I need to... | Tool | Note |
| --- | --- | --- |
| find a service | `list_services` | url, release id, replica count |
| one service in full | `service` | health check, env KEYS (never values), domain, replica ids |
| what has been deployed | `releases` | newest first, with `healthy` and the build each came from |
| put the previous release back | `rollback` | changes what is serving; confirm first |
| a replica's console | `logs` | pass a replica id from `service` |
| where a database is | `database` | engine, addresses and the machine to exec in; never a password |

## Each answer and what to do

| Answer | Meaning | Do |
| --- | --- | --- |
| `release_id` moved after a deploy | the new release is serving | report it; stop |
| `health_gate_failed`, 422 | the replica never answered | `diagnose` with `details.replica` (errors.md) |
| `conflict`, 409 | a rollout is already running on this service | retry once it finishes; `service` shows it |
| `not_found` | wrong id, or this key sees a different org | `list_services` shows what the key can see |

## Do not

- Do not roll back without confirming. It changes what is live for everyone.
- Do not read env values from `service`. They are never returned; that is deliberate (secrets.md).
- Do not `exec` a replica to read logs. `logs` is the console and needs no shell in the image.
- Do not ask for a database password over the API or MCP. There is no route that returns one: passwords live in the operator's own credentials file and never in the fleet. To open a session, tell the operator to run `pilot db connect`.
- Do not point a migration at `DATABASE_URL` on a pooled database. Transaction pooling drops the session state a migration relies on; use `DATABASE_URL_DIRECT`.
