# apps/dashboard

The pilots dashboard: accounts, orgs, API keys, usage, and the GitHub App's
product half. A [webjs](https://webjs.dev) app, deployed on `pilots.run` by the
platform it administers.

## What it is, and what it is not

**It mints keys. It never verifies one.** `POST /v1/api-keys` returns a
plaintext once and keeps the sha256; every host then authenticates a request
from its own local replica of that hash. Nothing in the request path of any
machine, service, or API call touches this app, so killing its host changes
nothing about whether the fleet answers. Do not add a "validate this key" call
here, and do not put this app in front of anything.

**It aggregates usage. It never meters it.** Each host answers
`GET /v1/usage?since=&until=` from its own ledger. A poller here reads every
live host once a minute and upserts what it says. A host that cannot reach this
app keeps metering; the dashboard catches up on its next tick. What the numbers
mean is the host's rule, not this app's: a suspended machine bills storage only,
so `machine_seconds` and `volume_gib_seconds` keep accruing while `vcpu_seconds`
and `mib_seconds` do not.

**It records repo connections. It never handles a webhook.** Delivery
verification, the exactly-one-host election, the build and PR previews are the
engine's (`apps/hostd/internal/github/handler.go`). This app writes the repo,
branch and autodeploy fields the engine reads, and renders whether the App is
actually installed on the repo's owner.

The store is deliberately small. Machines, services, volumes, domains and
releases are read from the fleet through `@pilots/sdk` on every request and
cached nowhere: a stale copy of fleet state is a second source of truth.

## Environment variables

Every one of these is read at boot and validated by `env.ts`, which fails fast
naming each bad variable rather than answering 502 on every page.

| Variable | Required | Where it comes from |
|---|---|---|
| `AUTH_SECRET` | yes, 32+ chars | Generate once: `node -e "console.log(require('crypto').randomBytes(32).toString('hex'))"`. Signs the session JWT. A guessable value means forgeable sessions, and this app mints fleet keys. |
| `AUTH_GITHUB_ID` | yes | The GitHub App's **client id**. Also the client id `pilot login`'s device flow ships in plaintext, and the id the CLI exchange authenticates tokens against. |
| `AUTH_GITHUB_SECRET` | yes | The GitHub App's **client secret**. Used for the OAuth code exchange and for the check-a-token call. |
| `PILOT_ADMIN_KEY` | yes | An `admin`-scoped fleet key. See **The admin key** below for how to get the first one and how to rotate it. |
| `PILOT_API_URL` | yes, no default | The fleet's API hostname, **off the workload apex**. The dashboard is a guest: it reaches hostd over this public address like any other client, never over loopback and never over `fdcc::`. |
| `DATABASE_URL` | yes | `file:/data/dev.db` in production, on the mounted volume. Locally, `file:./db/dev.db`. |
| `PILOT_GITHUB_APP_ID` | no | The App's numeric id. Absent, installation state renders as "App not configured on this fleet" and connecting a repo still works. |
| `PILOT_GITHUB_APP_KEY` | no | The App's private key: the PEM itself (as sealed `secret_env`) or a path to it (as on a host, in `/etc/pilots/config`). Both are accepted. |
| `PILOT_GITHUB_APP_SLUG` | no | The App's slug, for the "install it" deep link. Defaults to `pilots`. |
| `PILOT_USAGE_POLL` | no | `0` disables the usage poller. Set it on any instance that must not poll. |
| `PORT` | no | Defaults to 8080. |

`REDIS_URL` is deliberately **not** used. This app runs as a single replica, so
the in-memory store is correct for its cache, rate limiter and pub/sub. Adding
Redis would be a dependency bought for a scale shape the deploy does not have.

## The admin key

`PILOT_ADMIN_KEY` is admin-scoped because the dashboard has to list and act on
objects across every org it administers (`machines` is contained in `deploy`,
which is contained in `admin`).

**Bootstrap, once per fleet:**

```sh
ssh root@<host> /opt/pilots/bin/hostd bootstrap-key    # prints one admin key for org `ops`
```

Use that key for the first `pilot deploy` of this app, and as the sealed
`PILOT_ADMIN_KEY` on its service.

**Rotation, from then on:** mint a second admin key from the keys page,
redeploy with it, then revoke the bootstrap key. Revocation reaches every host
through the same replication the key itself does, so no host has to be
reachable from here for it to take effect.

The key is provisioned as a sealed `secret_env` entry on the dashboard's own
service. `pilot deploy` resolves each `secret://` value on the client and sends
it over TLS; hostd seals it with the fleet key before any row is written. It is
in no compose file, no image, and no row in the clear.

## Local development

```sh
cp apps/dashboard/.env.example apps/dashboard/.env    # then fill in the values
npm install                                            # from the repo root
npm run build:sdk-js                                   # @pilots/sdk resolves to dist/
npm run dev --workspace=apps/dashboard                 # http://localhost:8080
```

For sign-in to work locally, register a GitHub App (below) with
`http://localhost:8080/api/auth/callback/github` as an additional callback URL.

The usage poller does not run under `webjs dev`; it is gated to
`NODE_ENV=production`. Call `startUsagePoller` directly if you need to exercise
it, as `test/usage/poller.test.ts` does.

## The design system

Every page is built from `@webjsdev/ui`, the framework's shadcn-style kit. Its
source is copied into this repo, so these are ours to edit:

- `components/ui/` holds the class helpers (`buttonClass`, `badgeClass`,
  `tableCellClass`, ...). Add one with `npx webjsdev ui add <name>`; read the
  structure it expects with `npx webjsdev ui view <name>`.
- `lib/utils/ui.ts` holds the repeated *markup*: `pageHeading`, `lede`,
  `field`, `errorAlert`, and `dataTable`. The split is deliberate. A repeated
  class string belongs in `components/ui/`, a repeated chunk of markup belongs
  here, and neither ships any JavaScript.
- `app/layout.ts` holds the design tokens. `public/input.css` maps them into
  Tailwind utilities with `@theme`, so a token defined in neither place renders
  as nothing at all rather than as an error; `test/ui/design-system.test.ts`
  asserts every mapped token has a value on the served page.

Reach for the helpers rather than writing the utilities out. Two of them exist
mainly to keep an accessibility obligation from being forgotten per page:
`dataTable` requires a caption and emits `scope="col"` itself, and `field`
requires the id that its label points at.

Two local edits to the kit's own files are deliberate and commented in place:
`components/ui/checkbox.ts` keeps its checkmark CSS in `public/input.css`
instead of injecting it at module scope, which would ship a whole page's
JavaScript and skip the first paint, and it carries a `dark:checked:` pair the
registry lacks. Re-running `webjsdev ui add checkbox` overwrites both; run
`npx webjsdev ui diff` to see where local copies have drifted from upstream.

The stylesheet is compiled, not generated at runtime. `webjs dev` and
`webjs start` rebuild it, and `npm run css:build` does it by hand.

## Tests

```sh
npm run test:dashboard              # from the repo root: check, typecheck, server tests
npm run test:dashboard:browser      # the browser layer; needs Chromium
npx playwright install chromium     # first run only
```

The browser layer is a separate script because it needs a real Chromium, so
`npm test` stays green on a machine with no browsers installed. Nothing in it
is optional; each file is there because the thing it checks is invisible to the
server layer:

- `test/machines/browser/` applies a delta to the rendered machine list, which
  is post-hydration behaviour, so the served bytes are identical either way.
- `test/theme/browser/` drives the theme toggle, which writes `localStorage`
  and `<html>` attributes outside its own subtree, and measures the checkbox's
  computed colours in both themes, which needs the compiled stylesheet parsed.
- `test/ui/browser/` runs axe over the served pages and the live components, in
  both themes. Contrast is computed colour, so this is the only layer that can
  see it.

Run `npm run css:build` before the browser layer if the stylesheet is stale:
two of those files fetch `/public/tailwind.css` and assert it is served.

Every layer runs against a fake fleet installed on `globalThis.__pilots_fleet`
before the app boots, which `modules/fleet/client.server.ts` reads first. No
test needs a reachable host, and no test can reach one by accident.

## Registering the GitHub App

One App serves all three faces: this dashboard's OAuth login, the CLI's device
flow, and the engine's installation tokens and webhook. Create it once:

- **Callback URL** `https://pilots.run/api/auth/callback/github`
- **Enable Device Flow** on, which is what `pilot login` uses
- **Webhook URL** `https://<api host>/v1/github/webhook`, with a generated secret
- **Permissions** `contents: read`, `pull_requests: write`, `metadata: read`
- **Events** `push` and `pull_request`

Its client id and secret become this app's `AUTH_GITHUB_ID` and
`AUTH_GITHUB_SECRET`. Its app id, a generated private-key PEM and the webhook
secret go on every host in `/etc/pilots/config` as `PILOT_GITHUB_APP_ID`,
`PILOT_GITHUB_APP_KEY` (a path to the PEM) and `PILOT_GITHUB_WEBHOOK_SECRET`.
All three empty turns the engine's webhook route off.

## Deploying

```sh
cd apps/dashboard
PILOT_API_KEY=<the bootstrap key> pilot deploy
```

`pilots.run` is an ordinary custom domain on this service, and deliberately a
**separate apex** from `pilotrun.app`: a workload sharing the dashboard's apex
could set cookies scoped to it. Its DNS is A records to every host IP, DNS-only,
so HTTP-01 issuance can be answered by any host.

The image builds from the **repo root**, not from this directory, because the
app imports `@pilots/sdk` from the `sdks/js` workspace. The compose file sets
that context; the root `.dockerignore` keeps kernels, rootfs images and the Go
data plane out of it.

### Why the rate limiters trust the proxy

Every rate limiter in this app sets `trustProxy: true`, which it has to: the
workload router fronts the dashboard, so without it the socket peer is the
router on every request and the limit becomes a single shared bucket. That is
safe because the router deletes an inbound `X-Forwarded-For` at the public
entry before appending the peer, and sets `X-Forwarded-Proto` and
`X-Forwarded-Host` there too, so the leftmost entry is always an address the
edge observed rather than one a caller chose. The sibling headers a limiter
might fall back to -- `X-Real-IP`, `CF-Connecting-IP`, `True-Client-IP` and
RFC 7239 `Forwarded` -- are deleted at the same point, so there is no second
header to forge instead.
