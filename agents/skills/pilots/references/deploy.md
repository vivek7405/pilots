# Deploy

## What this covers

One directory or repository to a URL: the one-call path, what the platform detects, monorepos, the two Dockerfile rules, and what to do with each answer. Not covered: secrets (secrets.md), volumes (volumes.md), a failing deploy's diagnosis (errors.md).

## The command to copy

    pilot deploy                       # in the app's directory
    pilot deploy ./apps/web --app shop # a subdirectory, named

MCP: `deploy` with `{ "dir": "<absolute path>" }`. Nothing else is required.

## What happens

1. The directory is tarred, with `.dockerignore` honoured and `.git` skipped, and posted to `POST /v1/plan`.
2. The host answers with a plan: one step per service, each saying where it came from. Order, first hit wins: a compose file, then a `Dockerfile`, then a recipe, then `unknown`.
3. Each step is built with `POST /v1/builds`, the service is created or updated, the release is deployed and health-gated.
4. The result is `{ app, services: [{ name, url, release_id }], next }`.

## What the platform detects

| Signal | Framework | Health |
| --- | --- | --- |
| `package.json` with any `@webjsdev/*` dependency | webjs | `/__webjs/ready`, grace 40 s |
| `next.config.*` and a lockfile | next | `/` |
| `react-router.config.*` or `remix.config.*` | react-router | `/` |
| `package.json` with a bare `remix` dependency | remix (Remix 3) | `/` |
| `vite.config.*` | vite, static behind nginx | `/` |
| `manage.py` with `requirements.txt` or `pyproject.toml` | django | `/`, grace 30 s |
| `main.py` or `app.py` importing fastapi | fastapi | `/` |
| `Gemfile` with `bin/rails` | rails | `/up` |
| `go.mod` | go | `/` |
| `Cargo.toml` | rust | `/` |
| `composer.json` with `artisan` | laravel | `/` |

Every recipe sets `PORT=8080`, exposes 8080 and reads `$PORT`. The router dials 8080.

### Next.js: the deployment adapter

Next defines an adapter interface, and pilots implements it. Setting
`adapterPath: '@pilots/sdk/next'` in `next.config.js` (or
`NEXT_ADAPTER_PATH=@pilots/sdk/next` with no config change) makes the build
turn on `output: 'standalone'`, so the image carries the traced server rather
than the whole repository, and writes `<distDir>/pilots-deploy.json` holding
the static and prerendered paths, the routing rules, and a warning for any
route built for the edge runtime. Without it the generic recipe above still
works; the image is just much larger.

**Cron jobs come from the app's own config.** A `vercel.json` with `{ "crons": [{ "path": "/api/digest", "schedule": "0 5 * * *" }] }` is read for any app, whatever it is written in and whether or not it brought a Dockerfile; a webjs `package.json` may carry the same list under `"webjs": { "crons": [...] }`, and wins where both are present. The plan turns either into the service's schedules (services.md), so a cron needs no compose file and nothing pilots-specific. A `vercel.json` that does not parse is ignored; one that parses and spells a cron wrongly is a 400 naming the entry.

## Monorepos

A root `package.json` with `workspaces` deploys each workspace directory as a service named after it, built from the repository root with `WORKDIR /app/<dir>`. The install stays at the root, so the lockfile and the hoisted `node_modules` are the ones each workspace expects. A workspace no recipe recognises is skipped and named in the notes.

Any other layout is `unknown` at the root. Write a compose file with one `build:` per app (compose.md).

## The two Dockerfile rules

When you write a Dockerfile yourself:

1. bind `0.0.0.0`, never `127.0.0.1`;
2. read the port from `$PORT`, with 8080 as the fallback.

Both mistakes build cleanly and answer 502. `127.0.0.1` is right in exactly one place, a `HEALTHCHECK` probe, which runs inside the guest.

## Each answer and what to do

| Answer | Meaning | Do |
| --- | --- | --- |
| `services[].url` | deployed and healthy | report the URL; stop |
| `unknown_framework` | no compose file, no Dockerfile, no recipe | read `details.listing` and `details.manifests`, write a Dockerfile obeying `details.rules`, call `build` with it, then `deploy` with `name` and `build` |
| `plan_unsupported` | the compose file asks for something pilots does not do | fix each key in `details.unsupported`; every one is named |
| `plan_multi_service` | a push tried to deploy a repository with more than one app | commit a compose file and deploy it with `pilot deploy` |
| `build_failed` | the build stopped; every log line is in the error | read the line carrying `error`, fix the Dockerfile, `build` again |
| `health_gate_failed`, 422 | the app never answered its health check | `diagnose` with `details.replica`; it is usually the port or the bind address |
| `quota_exceeded`, 429 | the org is at a limit | `next` names the limit; destroy something or ask an admin |

## Do not

- Do not run `generate_dockerfile` before `deploy`. `deploy` already does it.
- Do not set `port`. The recipe sets 8080 and the platform passes it.
- Do not deploy a second time to retry a 422. Read `diagnose` first.
