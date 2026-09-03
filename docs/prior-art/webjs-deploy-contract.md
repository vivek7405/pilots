# webjs — what a deploy has to provide

_Research date: 2026-09-03, against the local clone at
`~/Documents/Projects/frameworks/webjs` (commit `4b8da0bc`). webjs is our own
buildless, web-components-first full-stack framework; the crisp reference
customer scaffolds webjs apps inside sandboxes and every dashboard/app on
pilots is a webjs app. This note is the runtime contract pilots' PaaS face
must satisfy so a webjs app deploys with zero per-platform config. Paths are
relative to the webjs repo root._

## The contract in one table

| Concern | What webjs does | What pilots must provide |
|---|---|---|
| Runtime | Node ≥ 24 (built-in `module.stripTypeScriptTypes`) or Bun (`amaro`). No lower Node — recursive `fs.watch` + the stripper need 24. | Golden rootfs ships Node 24 (already does, via the crisp env contract). Bun optional. |
| Build step | **None.** `.ts` served directly; Drizzle has no codegen. The scaffold `Dockerfile` is `npm install` + `COPY . .` + `npm start`. | The Dockerfile build path is enough; a webjs deploy is `npm install` + ship. There is nothing to bundle, so **the S3 layer cache is the whole win** — a redeploy is one changed source layer. |
| Start | `npm start` → `webjs start`, which runs `webjs.start.before` (`webjs db migrate`, idempotent) **in-process**, then serves. | Nothing. Migrations happen at boot, so a release snapshot taken *after* first-ready contains a migrated DB. |
| Port | `$PORT`, default 8080. `.env` is loaded by the CLI *before* the port is computed (`packages/cli/lib/port.js:231-263`). | Set `PORT` (or accept 8080); route `<name>.pilotrun.app` → 8080 — the same default sprites proxies. |
| Liveness | `GET /__webjs/health` → 200 as soon as listening. | Use for process-alive, not for rollout gating. |
| Readiness | `GET /__webjs/ready` → **503 `{"status":"pending"}` until fully warm** (analysis + first vendor attempt), then 200. Warm does not prove the DB is reachable; an app can add `readiness.ts` to gate on dependencies. | **The health-gated rollout must probe `/__webjs/ready`, not `/health`.** The scaffold HEALTHCHECK uses `--start-period=40s --retries=5 --interval=15s`; those are the defaults to adopt when a service declares no check. |
| Version | `GET /__webjs/version` — JSON describing the live build, answered before warm. | Deploy can assert which build is serving; cheap release-verification hook. |
| Shutdown | On SIGTERM/SIGINT: stop accepting, drain in-flight, close; exit 0 only on a clean operator stop. | Send SIGTERM and wait for the drain before killing the old replica in a rollout; keep the old one until the new one is *ready*. |
| TLS / HTTP/2 | **Framework speaks HTTP/1.1 only.** TLS + h2 are the edge's job; per-file ESM relies on h2 multiplex. HSTS is set only when `X-Forwarded-Proto: https` says the edge terminated TLS. | The router must terminate TLS, speak **HTTP/2 to the browser**, proxy HTTP/1.1 to the guest, and set `X-Forwarded-Proto` / `X-Forwarded-For`. Without h2 at the edge a webjs page loads noticeably slower (many small modules). |
| Security headers | Set by the framework (`X-Content-Type-Options`, `X-Frame-Options`, `Referrer-Policy`, `Permissions-Policy`, HSTS in prod). CSRF, CSP nonces built in. | Do not add or strip them at the proxy. |
| Timeouts | `requestTimeout` 30s, `headersTimeout` 20s, keep-alive tuned; overridable via `WEBJS_*_MS`. | Router idle/request timeouts should be ≥ these or long-running server actions get cut at the edge first. |
| Env / secrets | `.env` for dev only; production gets env from the platform. `AUTH_SECRET`, `AUTH_GITHUB_ID/SECRET`, `DATABASE_URL`, `REDIS_URL`. | Injected env + sealed secrets (5b) cover it. |
| Database | Default Drizzle + `node:sqlite` (**no native module**), file at `db/dev.db`; `.dockerignore` excludes the db so **the runtime volume owns the file**. Postgres via `DATABASE_URL`. | SQLite apps need a **volume mounted at the app's `db/` path** (or the whole app dir) to survive redeploy; Postgres apps use the compose fragment over `.internal`. |
| Horizontal scale | Cache, sessions, auth, WS broadcast, rate limit share one in-memory store; `setStore(redisStore({url: REDIS_URL}))` moves all of them to Redis. | N-replica services of a webjs app **require `REDIS_URL`** (a compose fragment) unless the app is stateless; single-replica + SQLite is the default sweet spot. |
| Importmap vendor | `.webjs/vendor/` (committed) holds the importmap so boot never reaches `api.jspm.io`; everything else under `.webjs/` is a per-machine cache. | Egress to jspm is not required at boot if vendor is committed; keep it allowed anyway for `webjs dev` inside sandboxes. |
| Logs | One structured JSON line per request (method, path, status, durationMs, requestId) to stdout. | `GET /v1/machines/:id/logs?follow` streams stdout — nothing to parse. |
| Dev mode in a sandbox | `npm run dev` (live reload via SSE, `fs.watch`). Must start via `npm run dev`, **not** `npx webjs dev` (memory: sprites-preview-port-8080-dns). | A sandbox previewing a webjs app is just port 8080 + SSE passthrough on the router (no buffering of `text/event-stream`). |

## Why this makes webjs the ideal pilots workload

- **Build = `npm install`.** With the fleet-wide S3 layer cache warm, a webjs
  redeploy touches one layer. Combined with 5c's *release snapshots* (deploys
  restore a memory image instead of booting), the deploy path can be
  `push → build one layer → restore → /__webjs/ready` with nothing else in
  between. This is the "extremely fast to deploy web apps" target and no
  competitor optimises for it: Fly builds on remote builders then boots,
  sprites has no deploy face, e2b has no deploy face.
- **Readiness is standardised.** Every webjs app answers `/__webjs/ready`; the
  PaaS never needs a per-app health-check declaration for webjs apps. Detect
  `@webjsdev/cli` in `package.json` and default the check to that path.
- **Buildless + snapshot = instant sandbox iteration.** In the crisp loop, an
  agent edits a `.ts` file and the page refreshes; there is no rebuild to
  wait for, so per-message checkpoints (5c) are the only latency in the loop.

## Gotchas to keep

- `/__webjs/ready` is a real 503 until warm; a rollout that treats *any* HTTP
  answer as healthy will cut over to a cold instance. Gate on 200.
- webjs assumes a trusted edge for `X-Forwarded-Proto`; the router must set
  it or HSTS silently never turns on.
- SQLite on a JuiceFS volume: `node:sqlite` uses normal file locking; a single
  replica is fine, two replicas on one shared volume are not.
- HTTP/2 to the guest is unnecessary and unsupported; h2 stops at the router.

## Where to look

| Question | Path |
|---|---|
| Production image shape | `packages/cli/templates/Dockerfile` |
| What ships / what is excluded | `packages/cli/templates/.dockerignore` |
| Port + `.env` load order | `packages/cli/lib/port.js` |
| `start.before` / migrate-at-boot | `packages/cli/lib/app-tasks.js` |
| Bun rewrite of the scaffold | `packages/cli/lib/runtime-rewrite.js` |
| Deployment doc (probes, shutdown, h2, timeouts, logs) | `website/app/docs/deployment/page.ts` |
| Auth surface used by the dashboard | `.agents/skills/webjs/references/auth-and-sessions.md` |
