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

An empty key throws before any request is made.

## Methods

One method per route, grouped by the noun it acts on.

| Call | Route |
| --- | --- |
| `health()` | `GET /v1/health` |
| `machines.create(req)` `.list()` `.get(id)` `.destroy(id)` | `/v1/machines` |
| `machines.exec(id, req)` | `POST /v1/machines/{id}/exec` |
| `machines.execStream(id, argv, opts)` | `GET /v1/machines/{id}/exec/stream` (WebSocket) |
| `machines.logs(id)` `.followLogs(id)` | `GET /v1/machines/{id}/logs` |
| `machines.suspend(id)` `.wake(id)` `.stop(id)` `.start(id)` | `POST /v1/machines/{id}/…` |
| `machines.checkpoint(id, {comment})` `.listCheckpoints(id)` | `/v1/machines/{id}/checkpoints` |
| `machines.promote(id, req)` | `POST /v1/machines/{id}/promote` |
| `machines.volume(id)` | `GET /v1/machines/{id}/volume` |
| `checkpoints.restore(id)` `.get(id)` | `/v1/checkpoints/{id}` |
| `builds.create(tar)` `.logs(id, {follow})` | `/v1/builds` |
| `services.create(req)` `.list()` `.get(id)` `.patch(id, req)` | `/v1/services` |
| `services.deploy(id, req)` `.rollback(id)` `.releases(id)` | `/v1/services/{id}/…` |
| `domains.add(req)` `.list()` `.remove(hostname)` | `/v1/domains` |
| `volumes.create(req)` `.list()` | `/v1/volumes` |
| `hosts.list()` | `GET /v1/hosts` |
| `apiKeys.create(req)` `.revoke(hash)` `.list(org)` | `/v1/api-keys` |
| `quotas.get(org)` `.put(org, quota)` | `/v1/quotas/{org}` |
| `usage.get({since, until})` | `GET /v1/usage` |
| `compose.plan({compose, env})` | `POST /v1/compose/plan` |

Every wire type is exported under the name hostd's Go struct carries, with the
JSON tags as its properties. The types from hostd's compose package carry a
`Compose` prefix, so `compose.Step` is `ComposeStep`. A test in this package
parses hostd's source on every run and fails when the two sides drift.

## Errors

Every non-2xx throws. The subclass tells you what to do about it.

| Class | When | Carries |
| --- | --- | --- |
| `PilotsError` | any failure | `status`, `body`, `message` (the body's `error`) |
| `NotFoundError` | 404 | as above |
| `QuotaExceededError` | 429 | `quota`, `limit`, `used`, `scope` |
| `ComposePlanError` | a compose plan hostd will not accept | `unsupported: [{service, key, message}]` |
| `BuildFailedError` | a build that failed | `buildId`, `lines` |

`quota` names which ceiling was hit, so a caller raises the right one rather
than guessing from a sentence. `scope` is `"host"` when the limit is the
host's rather than the org's, which is how builds are limited.

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

**A text verdict follows the binary exit frame.** hostd sends
`{"type":"exit","exit_code":n}` after frame `3`, because the binary frame
carries the code in one byte and truncates anything above 255. Whichever
arrives first decides; the other is ignored.

**An exec that names no user runs as `sprite`.** The guest image bakes that
account at uid 1000 with home `/home/sprite` and Node 24 on `PATH`, so a
command needs neither a `user` nor a `cwd` to land where these examples assume.

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
