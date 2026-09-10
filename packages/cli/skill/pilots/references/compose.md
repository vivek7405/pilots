# Compose

## What this covers

Describing more than one service: a web app with a database, a worker, a shared app name, volumes and secret references. Not covered: single-directory deploys, which need no file at all (deploy.md).

## The command to copy

    pilot deploy            # picks up compose.yaml in the directory

A file that covers the common case:

    name: shop
    services:
      web:
        build: .
        environment:
          DATABASE_URL: secret://database_url
        depends_on: [postgres]
      postgres:
        image: postgres:17
        volumes:
          - pgdata:/var/lib/postgresql/data
    volumes:
      pgdata:
        driver_opts:
          size: 10G

## What happens

1. The file's text and the directory's `.env` go to `POST /v1/compose/plan`. The CLI interpolates nothing; one parser, in Go, beside the daemon.
2. The plan is an ordered list of steps: build, volume, service, deploy, per service, in dependency order.
3. Services in the same app find each other at `<name>.internal`, which is why `DATABASE_URL` above points at `postgres.internal`.

## The `x-pilots` keys

Everything pilots needs that compose has no word for goes under `x-pilots:` on a service (or at the top level for `app`). Unknown keys are ignored, so a typo is silent: copy these.

| Key | Meaning |
| --- | --- |
| `domain` | the label the URL is minted from (default: the service name) |
| `private: true` | no URL; peers reach it at `<name>.internal` |
| `custom_domain` | a hostname you own (domains.md) |
| `auto_stop: off\|suspend` | `suspend` (default) sleeps an idle replica; `off` never does |
| `auto_start: true\|false` | wake on a request (default true; `false` is a dead URL) |
| `min_machines_running` | replicas kept resident, default 0 (scale to zero) |
| `soft_limit` | concurrent requests per replica before another starts (default 20) |
| `idle_timeout` | quiet before a replica sleeps, `30m` or seconds, 1s..1h (default 60s) |
| `schedules` | cron jobs, a list of `{ cron, path }` or `{ cron, cmd }` (services.md); `[]` clears the previous release's |
| `size_gib` | a volume's size, on the volume |
| `pre_deploy` | a command run on a throwaway machine before any replica is replaced (a migration) |
| `app` | top level only: the app name |

## The rules

| Rule | Why |
| --- | --- |
| the `.env` FILE, never the process environment | a deploy has to be reproducible from the checkout |
| one `build:` context per service | each service is its own image |
| a volume-backed service runs one replica | a volume is mounted by one machine at a time |
| a `secret://` reference, never a value | the file is committed (secrets.md) |

## Each answer and what to do

| Answer | Meaning | Do |
| --- | --- | --- |
| `plan_unsupported` | the file asks for something pilots does not do | fix every key in `details.unsupported`; they are all listed at once, so one pass is enough |
| `compose_invalid` | the file does not parse, or a variable is unset | the message names the file and the problem |
| `bad_request` about the app name | nothing names the app | add a top-level `name:`, set `COMPOSE_PROJECT_NAME`, or pass `--app` |

## Do not

- Do not write a compose file for a single app the platform already recognises. `deploy` handles it.
- Do not fix one unsupported key at a time. Every one is in the answer.
- Do not interpolate variables yourself. Put them in `.env` and let the planner do it.
