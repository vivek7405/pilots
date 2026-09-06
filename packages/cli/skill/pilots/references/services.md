# Services

## What this covers

A durable thing behind a permanent URL: finding one, reading its releases, rolling back, reading a replica's logs, and what the health gate is doing. Not covered: getting the first URL (deploy.md), why a deploy failed (errors.md).

## The command to copy

    pilot services ls
    pilot services info web

MCP: `list_services`, then `service` with `{ "service": "web" }`.

## What happens

A service is one or more machines behind a URL that never changes. A deploy builds a rootfs, creates a release, starts a replica from it, and lets the release take traffic only once the replica has passed its health check. That gate is why a broken deploy does not take the previous one down.

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
