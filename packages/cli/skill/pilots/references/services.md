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

A webjs app needs none of that: it declares crons in its own `package.json` (`"webjs": { "crons": [{ "path": "/jobs/digest", "schedule": "0 5 * * *" }] }`) and `deploy` reads them. Any other framework uses the compose keys above; a sandbox takes `pilot machines create --schedule "0 5 * * * /jobs/digest"`.

What the handler sees is a `GET` carrying `X-Pilot-Cron: <expression>`. The public edge strips that header from every outside request, so `if (!req.headers['x-pilot-cron']) return 403` is the whole check, with no secret to keep. A job can fire twice in rare cases (a host restart inside its minute), so make it idempotent; a job still running when its next minute comes is skipped, not overlapped. One replica fires for a service, however many it has. To remove every cron, deploy with `schedules: []` (or `crons: []`); an absent key keeps the previous release's.

A replica with no traffic suspends after about 30 seconds of quiet and the next request wakes it; that is the default and it costs nothing while asleep. A worker that must keep running with nothing connected to it -- a queue consumer, a scheduler -- keeps one replica resident with `x-pilots: min_machines_running: 1` in its compose entry (or `auto_stop: off`). The scale-down window is the autoscaler's and is not a knob; `idle_timeout` is the sandbox timer (sandboxes.md). The keys are listed in compose.md.

## The tools

| I need to... | Tool | Note |
| --- | --- | --- |
| find a service | `list_services` | url, release id, replica count |
| one service in full | `service` | health check, env KEYS (never values), domain, replica ids |
| what has been deployed | `releases` | newest first, with `healthy` and the build each came from |
| put the previous release back | `rollback` | changes what is serving; confirm first |
| a replica's console | `logs` | pass a replica id from `service` |

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
