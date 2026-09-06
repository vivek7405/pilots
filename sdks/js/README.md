# `@pilots/sdk`

The typed JavaScript client for the pilots API: instant sandboxes and durable
production services on one primitive, Firecracker microVMs.

Zero runtime dependencies. ESM only. Node 22 or newer, Bun, or Deno: the
streaming exec is built on `node:stream`, so a browser needs a bundler that
shims it.

```
npm i @pilots/sdk
```

## Construction

```ts
import { PilotsClient } from '@pilots/sdk'

const pilots = new PilotsClient(process.env.PILOT_API_KEY!)
const machine = await pilots.machines.create({ name: 'demo' })
console.log(machine.url) // https://demo.pilotrun.app
```

The base URL is read from `opts.baseURL`, then `PILOT_API_URL`, then
`https://api.pilotrun.app`. Every host serves the identical API, so any host in
the fleet is a valid endpoint: there is no control-plane tier to be down, and a
write that arrives at the wrong host is forwarded by hostd itself.

| Option | Default | Notes |
| --- | --- | --- |
| `baseURL` | `PILOT_API_URL` or `https://api.pilotrun.app` | Trailing slashes are stripped. |
| `fetch` | `globalThis.fetch` | Wrap it to add retries, pooling or tracing. |
| `timeoutMs` | `30000` | JSON calls only. Builds, log follows and streams get no deadline. |
| `WebSocket` | `globalThis.WebSocket` | The seam for the `ws` package on an older runtime. |
| `org` | none | Makes an ADMIN key act as one org. Every request carries `?org=`. |

`org` is for a process that serves many orgs from one operator key. hostd reads
it as the org to create rows in, charge quota to, and narrow every read by, so
the rows the process creates belong to the person who asked for them. A
tenant-scoped key already has exactly one org, so hostd ignores the parameter
there and setting it changes nothing.

An empty key throws before any request is made.

## Methods

One method per route, grouped by the noun it acts on.

| Call | Route |
| --- | --- |
| `health()` | `GET /v1/health` |
| `whoami()` | `GET /v1/whoami` |
| `machines.create(req)` `.list()` `.get(id)` `.destroy(id)` | `/v1/machines` |
| `machines.exec(id, req)` | `POST /v1/machines/{id}/exec` |
| `machines.execStream(id, argv, opts)` | `GET /v1/machines/{id}/exec/stream` (WebSocket) |
| `machines.logs(id)` `.followLogs(id)` | `GET /v1/machines/{id}/logs` |
| `machines.suspend(id)` `.wake(id)` `.stop(id)` `.start(id)` | `POST /v1/machines/{id}/…` |
| `machines.checkpoint(id, {comment})` `.listCheckpoints(id)` | `/v1/machines/{id}/checkpoints` |
| `machines.promote(id, req)` | `POST /v1/machines/{id}/promote` |
| `machines.volume(id)` | `GET /v1/machines/{id}/volume` |
| `checkpoints.restore(id)` `.get(id)` | `/v1/checkpoints/{id}` |
| `builds.create(tar)` `.createFromRepo(ref)` `.logs(id, {follow})` | `/v1/builds` |
| `services.create(req)` `.list()` `.get(id)` `.patch(id, req)` | `/v1/services` |
| `services.deploy(id, req)` `.rollback(id)` `.releases(id)` | `/v1/services/{id}/…` |
| `domains.add(req)` `.list()` `.remove(hostname)` | `/v1/domains` |
| `volumes.create(req)` `.list()` | `/v1/volumes` |
| `hosts.list()` | `GET /v1/hosts` |
| `apiKeys.create(req)` `.revoke(hash)` `.list(org)` | `/v1/api-keys` |
| `quotas.get(org)` `.put(org, quota)` | `/v1/quotas/{org}` |
| `usage.get({since, until})` | `GET /v1/usage` |
| `compose.plan({compose, env})` | `POST /v1/compose/plan` |

`services.patch` replaces rather than merges: `env`, `secret_env` and
`replicas` overwrite what is stored and take effect at the next deploy. `knobs`
are refused there with a 400 naming the field and travel on `services.deploy`.
`volume` on a service create is create-only and pins `replicas` to one; the
patch refuses it as an unknown field and refuses `replicas` above one on a
service that mounts a volume.

A service read carries `depends_on`, the sibling services in the same app whose
`<name>.internal` address this one's environment references. hostd derives it on
every read from both halves of the environment and stores it nowhere, so it says
what the service is configured to dial right now. Names only, never values.
`usage.get` answers for the ONE host it reached, so a fleet is the sum of a
call to each; a suspended machine bills storage only.

Every wire type is exported under the name hostd's Go struct carries, with the
JSON tags as its properties. The types from hostd's compose package carry a
`Compose` prefix, so `compose.Step` is `ComposeStep`. A test in this package
parses hostd's source on every run and fails when the two sides drift.

`health()` carries `store_version`, the sum of that host's replica version
vector: how many changes, from every host, it has applied. Comparable across
hosts, so two hosts far apart on it are a replication problem. 0 on a
single-box SQLite host, which has no replica.

Every machine carries `last_start` and `last_start_at`, which say how it last
came up:

| `last_start` | What happened |
| --- | --- |
| `restore` | its memory image was resumed -- the fast path, and the usual one |
| `boot` | a kernel boot, which a create with an image or a volume pays once, and every redeploy |
| `cold_boot` | a restore that was **downgraded**: no host of the memory image's CPU vendor was alive, so the machine booted from its own disk |

A cold boot keeps the id, name, URL, volume, agent token and every byte on
disk. It loses running processes, everything in memory, and open connections.
It is automatic and uniform -- availability wins over continuity -- so a client
that cares reads this field rather than a knob it can set. A machine that has
not started since the field existed reports neither.

`health()` also carries `cpu_vendor`, which pool that host restores memory
images from, and `hosts()` carries each host's, so the fleet's split is visible
without a shell on every box.

## The front door

```ts
const { plan, detected } = await client.plan(tarOfMyDirectory, { app: 'shop' })
```

`plan()` posts a tar of a directory and answers with the plan an executor runs
plus one `detected` entry per step saying where it came from: a compose file, a
`Dockerfile`, or a framework recipe. It is on the client rather than under
`compose` or `services` because it is what a caller reaches for before it knows
which of those a directory is.

```ts
const { plan } = await client.planRepo({ repo: 'you/shop', ref: 'main' }, { app: 'shop' })
```

`planRepo()` names a repository instead of sending one. The host fetches the
ref through the fleet's GitHub App, the same path a push takes, so a caller
that holds no repository bytes can still plan. A fleet with no App configured
answers `not_configured` and says to send a tar instead.

## Errors

Every non-2xx throws. The subclass tells you what to do about it.

| Class | When | Carries |
| --- | --- | --- |
| `PilotsError` | any failure | `status`, `body`, `message`, `code`, `next`, `details` |
| `NotFoundError` | 404 | as above |
| `QuotaExceededError` | 429 | `quota`, `limit`, `used`, `scope` |
| `ComposePlanError` | a compose plan hostd will not accept | `unsupported: [{service, key, message}]` |
| `BuildFailedError` | a build that failed | `buildId`, `lines` |
| `HealthGateError` | 422, a release that never became healthy | `details: {service, replica, release, grace_sec, last}` |
| `UnknownFrameworkError` | 400, a directory the platform cannot place | `details: {dir, looked_for, listing, manifests, workspaces, rules}` |

Three fields are on the base class and therefore on every error. `code` is a
stable snake_case noun to branch on, from the closed list in
`apps/hostd/internal/api/errors.go`. `next` is the one thing to do about it,
naming the command or the call. `details` is typed per code. They are on the
base rather than only on the subclasses so that a code this version has never
heard of still reaches the caller with its next step attached, instead of being
dropped on the way through.

The last two are matched on `code` and never on the status alone: 422 is the
shape of the health gate's answer today, and a later 422 for something else
must not arrive typed as this one.

`quota` names which ceiling was hit, so a caller raises the right one rather
than guessing from a sentence. `scope` is `"host"` when the limit is the
host's rather than the org's, which is how builds are limited.

`HealthGateError`'s `details` carry NO address. The probe target is the host's
own view of the replica, inside a network namespace, and it is not reachable
from wherever the error is being read.

## Streaming exec

```ts
const stream = pilots.machines.execStream('m-…', ['bash', '-c', 'npm run build'], {
  cwd: '/home/sprite/app',
  env: { NODE_ENV: 'production' },
})
stream.stdout.pipe(process.stdout)
stream.stderr.pipe(process.stderr)
const code = await stream.wait()
```

Three things about this are worth knowing before you rely on it.

**`stdin` is false by default.** A process holding an open stdin it never reads
hangs, and an agent run under `claude -p` is exactly such a process. Pass
`{stdin: true}` to opt in, then use `writeStdin(chunk)` and `endStdin()`; both
throw otherwise.

**A text verdict LEADS the binary exit frame.** hostd sends
`{"type":"exit","exit_code":n}` and then frame `3`, in that order, because the
binary frame carries the code in one byte: a command killed by a signal has an
exit code of -1, which one byte reports as 255 and no reader can tell from a
command that genuinely exited 255. `wait()` settles on whichever verdict arrives
first and closes the socket, so the text one has to be first for the
untruncated code to be the one you get. Frame `3` still follows it, unchanged,
for a client that reads only binary frames.

**An exec that names no user runs as `sprite`.** The guest image bakes that
account at uid 1000 with home `/home/sprite` and Node 24 on `PATH`, so a
command needs neither a `user` nor a `cwd` to land where these examples assume.

### An interactive terminal

`tty: true` runs the command on a pseudo-terminal, which is what an interactive
shell, `tmux` and `vim` need and what three pipes cannot give them.

```ts
const term = pilots.machines.execStream('m-…', ['bash', '-l'], {
  tty: true,
  rows: 40,
  cols: 120,
})
term.stdout.pipe(process.stdout)
term.writeStdin('ls\n')
term.resize(100, 30)
```

It is a mode on the same stream, not a second protocol: the frames, the ids and
the exit verdict are identical. Four things change, and only under `tty`.

- A PTY has one device, so everything the command writes arrives on `stdout`
  and `stderr` never produces a byte.
- `stdin` is implied and forced on. `{tty: true, stdin: false}` contradicts
  itself and hostd answers it with a 400 before the machine is even woken.
- `endStdin()` sends EOT (`0x04`) to the terminal instead of closing an input,
  because a terminal has no separate write end to close. The session stays
  open: what EOT means is the shell's decision.
- `resize(cols, rows)` sends `{"type":"resize","cols":N,"rows":N}`. It throws
  on a stream opened without `tty`.

`rows` and `cols` set the initial window, default 24 by 80, each 1..65535. A
value outside that closes the socket with 1008 rather than being clamped.

**A close with no exit frame is an error.** `wait()` rejects rather than
resolving 0. The guest agent drains both output pumps before writing the exit
frame and websocket frames are ordered, so an exit frame means every byte that
preceded it has already arrived. A socket that dropped instead means nobody
knows what the command did.

**An unread stream grows.** `stdout` and `stderr` are `PassThrough`s, and a
WebSocket cannot be paused, so those buffers are the only boundary. Read them,
or use the buffered `machines.exec` for output nobody intends to read.

The key travels as the `authorization.bearer.<key>` subprotocol rather than a
header, because browsers cannot set handshake headers and one code path is
easier to get right than two. hostd accepts either form. On a runtime with no
global `WebSocket`, pass one through `ExecStreamOptions.WebSocket`.

## Builds

```ts
const build = await pilots.builds.create(tarStream)
for await (const line of build) console.log(line.step, line.line)
```

Or, when only the outcome matters:

```ts
const rootfsBuildId = await (await pilots.builds.create(tarStream)).result()
```

hostd answers 200 before the build starts, so a ten-minute build is watchable
while it runs. That means the status code cannot be the verdict: the last line
is. `result()` reads it, and throws `BuildFailedError` both when that line
carries an error and when the stream ended with no verdict at all.

`close()` walks away from a build without draining it, releasing the socket
rather than holding it until GC. A `result()` afterwards throws, because a
stream nobody finished has no verdict and an abandoned build must never read as
a successful one.

```ts
const build = await pilots.builds.createFromRepo({ repo: 'you/shop', ref: 'main' })
```

`createFromRepo()` builds a repository by naming it. The host fetches the ref
through the fleet's GitHub App, plans it, and builds the one step a plan may
produce. A plan with more than one step is refused with `plan_multi_service`,
readable at the build's own log.

## `@pilots/sdk/sprites-compat`

A sprites-shaped face over the same client, so a codebase written against the
sprites SDK moves by changing one import line.

```ts
import { SpritesClient, type ExecResult } from '@pilots/sdk/sprites-compat'

const client = new SpritesClient(process.env.PILOT_API_KEY!, { timeout: 300_000 })
const sprite = await client.createSprite('demo')
const out: ExecResult = await sprite.execFile('bash', ['-c', 'ls'], { cwd: '/home/sprite/app' })
```

Four rules decide the shapes here.

- **`sprite.id` is the machine's NAME.** A sprites consumer persists the id and
  hands it straight back as a path segment, and the alias serving those paths
  resolves names. `sprite.machineId` carries the `m-…` id for calls made
  through the typed client. Either form works as an argument: a value that
  matches no name but looks like a machine id is looked up as one.
- **`restoreCheckpoint` restores in place.** Exactly one request,
  `POST /v1/checkpoints/{id}/restore`, and no machine is created. A machine
  created in a restore would get a new URL, and a URL is permanent.
- **`createCheckpoint` and `restoreCheckpoint` return a `Response`.** Its body
  is one NDJSON line, which is what a sprites consumer reads with `.text()` and
  scans backwards for `id`.
- **`setPublicUrl` is a no-op.** A workload's URL is public here by default, so
  there is nothing to switch on.

`spawn` is synchronous, as the sprites SDK's is, so it needs a sprite that has
already been resolved: use `createSprite` or `getSprite` rather than the lazy
`client.sprite(name)`.

## Porting crisp from `@fly/sprites`

The reference customer's coupling to its provider is one file,
`lib/sprites-client.ts`. The whole port is six changes.

1. `package.json`: `"@fly/sprites": "^0.0.1"` becomes `"@pilots/sdk": "^0.1.0"`.
2. `lib/sprites-client.ts:1-2`: the two imports become
   `import { SpritesClient as OfficialSpritesClient, type ExecResult } from '@pilots/sdk/sprites-compat'`.
3. `lib/sprites-client.ts` `setPublicUrl`: the body becomes
   `await this.client.setPublicUrl(spriteName)`. It cannot stay as it is: the
   current body is a raw `fetch` to a hard-coded `https://api.sprites.dev`,
   which no adapter can intercept and which answers 401 to a pilots key.
4. `lib/sprites-client.ts` `getSpritesClient`: read `PILOT_API_KEY` instead of
   `SPRITES_TOKEN`, and name it in the error text. The constructor call is
   unchanged, because the adapter reads `PILOT_API_URL` itself.
5. `modules/sprites/actions/create-sprite.ts`: drop the five nvm lines. Node 24
   is on the image. `/home/sprite/app` stays.
6. `.env.example`: the `PILOTS_API_URL` / `PILOTS_TOKEN` / `SPRITES_TOKEN`
   block becomes `PILOT_API_URL=` and `PILOT_API_KEY=`.

`StreamingCommand` and `spawnStreaming` are untouched. They build their own
WebSocket URL against `/v1/sprites/{name}/exec` and send the key in a header,
which is exactly what hostd's name-keyed alias serves, across hosts included.
