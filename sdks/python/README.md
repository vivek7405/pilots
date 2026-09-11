# `pilots-sdk`

The typed Python client for the pilots API: instant sandboxes and durable
production services on one primitive, Firecracker microVMs.

Two dependencies, `httpx` and `websockets`. Python 3.10 or newer. Synchronous
throughout: every call returns when the server has answered, and the streams
are iterators. The framework adapters wrap it for the frameworks that are
async.

```
pip install pilots-sdk
```

## Construction

```python
from pilots import PilotsClient

pilots = PilotsClient()  # reads PILOT_API_KEY and PILOT_API_URL
machine = pilots.machines.create(name="demo")
print(machine.url)  # https://demo.pilotrun.app
```

The key comes from the argument, then `PILOT_API_KEY`. The base URL comes from
`base_url`, then `PILOT_API_URL`, then `https://api.pilotrun.app`. Every host
serves the identical API, so any host in the fleet is a valid endpoint: there is
no control-plane tier to be down, and a write that arrives at the wrong host is
forwarded by hostd itself.

| Argument | Default | Notes |
| --- | --- | --- |
| `api_key` | `PILOT_API_KEY` | An empty key raises before any request is made. |
| `base_url` | `PILOT_API_URL` or `https://api.pilotrun.app` | Trailing slashes are stripped. |
| `timeout` | `30.0` | Seconds, JSON calls only. Builds, log follows, deploys and streams get no deadline. |
| `org` | none | Makes an ADMIN key act as one org. Every request carries `?org=`. |
| `http_client` | a fresh `httpx.Client` | The seam for retries, pooling or tracing. |

## Methods

One method per route, grouped by the noun it acts on. Requests take a
dataclass from `pilots` (`CreateMachineRequest`, …), a plain dict, or keyword
arguments where the signature says so; responses are dataclasses named after
hostd's own structs, with the JSON tags as fields.

| Call | Route |
| --- | --- |
| `health()` | `GET /v1/health` |
| `whoami()` | `GET /v1/whoami` |
| `machines.create(req \| **fields)` `.list()` `.get(id)` `.update(id, req)` `.destroy(id)` | `/v1/machines` |
| `machines.exec(id, cmd=…, cwd=…, env=…, user=…, timeout_ms=…)` | `POST /v1/machines/{id}/exec` |
| `machines.exec_stream(id, argv, cwd=…, env=…, user=…, stdin=…, tty=…)` | `GET /v1/machines/{id}/exec/stream` (WebSocket) |
| `machines.logs(id)` `.follow_logs(id)` | `GET /v1/machines/{id}/logs` |
| `machines.suspend(id)` `.wake(id)` `.stop(id)` `.start(id)` | `POST /v1/machines/{id}/…` |
| `machines.checkpoint(id, comment)` `.list_checkpoints(id)` | `/v1/machines/{id}/checkpoints` |
| `machines.promote(id, req)` | `POST /v1/machines/{id}/promote` |
| `machines.volume(id)` | `GET /v1/machines/{id}/volume` |
| `checkpoints.restore(id)` `.get(id)` | `/v1/checkpoints/{id}` |
| `builds.create(tar, deploy=…)` `.create_from_repo(ref)` `.logs(id, follow=…)` | `/v1/builds` |
| `services.create(req)` `.list()` `.get(id)` `.patch(id, req)` | `/v1/services` |
| `services.deploy(id, req)` `.rollback(id)` `.releases(id)` | `/v1/services/{id}/…` |
| `domains.add(req)` `.list()` `.remove(hostname)` | `/v1/domains` |
| `volumes.create(req)` `.list()` | `/v1/volumes` |
| `hosts.list()` | `GET /v1/hosts` |
| `api_keys.create(req)` `.revoke(hash)` `.list(org)` | `/v1/api-keys` |
| `repos.connect(repo)` `.list()` | `/v1/repos` |
| `quotas.get(org)` `.put(org, quota)` | `/v1/quotas/{org}` |
| `usage.get(since=…, until=…)` | `GET /v1/usage` |
| `compose.plan(req)` | `POST /v1/compose/plan` |
| `plan(tar, app=…)` `plan_repo(ref, app=…)` | `POST /v1/plan` |

`services.patch` replaces rather than merges: `env`, `secret_env` and
`replicas` overwrite what is stored and take effect at the next deploy. `knobs`
are refused there with a 400 naming the field and travel on `services.deploy`.

Every wire type is exported under the name hostd's Go struct carries, with the
JSON tags as its fields. The types from hostd's compose package carry a
`Compose` prefix. `tests/test_drift.py` parses hostd's source on every run and
fails when the two sides drift.

Every machine carries `last_start`: `restore` (its memory image was resumed,
the usual path), `boot` (a kernel boot: a create with an image or a volume, and
every redeploy), or `cold_boot` (a restore downgraded because no host of the
image's CPU vendor was alive; the id, name, URL, volume and disk are kept, the
processes are not).

## Errors

Every non-2xx raises. The subclass tells you what to do about it.

| Class | When | Carries |
| --- | --- | --- |
| `PilotsError` | any failure | `status`, `body`, `message`, `code`, `next`, `details` |
| `NotFoundError` | 404 | as above |
| `QuotaExceededError` | 429 | `quota`, `limit`, `used`, `scope` |
| `ComposePlanError` | a compose plan hostd will not accept | `unsupported: [{service, key, message}]` |
| `BuildFailedError` | a build that failed | `build_id`, `lines` |
| `HealthGateError` | 422, a release that never became healthy | `details: {service, replica, release, grace_sec, last}` |
| `UnknownFrameworkError` | 400, a directory the platform cannot place | `details: {dir, looked_for, listing, manifests, workspaces, rules}` |

`code`, `next` and `details` are on the base class and therefore on every
error. `code` is a stable snake_case noun to branch on, from the closed list in
`apps/hostd/internal/api/errors.go`. `next` is the one thing to do about it.
They are on the base rather than only on the subclasses so that a code this
version has never heard of still reaches the caller with its next step attached.

The last two are matched on `code` and never on the status alone.

## Streaming exec

```python
stream = pilots.machines.exec_stream("m-…", ["bash", "-c", "npm run build"], cwd="/home/pilot/app")
for chunk in stream.stdout:
    sys.stdout.buffer.write(chunk)
code = stream.wait()
```

or, when the whole output is wanted at once:

```python
out, err, code = pilots.machines.exec_stream("m-…", ["ls", "-la"]).output()
```

Three things about this are worth knowing before you rely on it.

**`stdin` is False by default.** A process holding an open stdin it never reads
hangs, and an agent run under `claude -p` is exactly such a process. Pass
`stdin=True` to opt in, then use `write_stdin(chunk)` and `end_stdin()`; both
raise otherwise.

**A text verdict LEADS the binary exit frame.** hostd sends
`{"type":"exit","exit_code":n}` and then frame `3`, because the binary frame
carries the code in one byte and a command killed by a signal (-1) cannot be
told from 255 there. `wait()` settles on whichever verdict arrives first.

**An exec that names no user runs as `pilot`.** The guest image bakes that
account at uid 1000 with home `/home/pilot` and Node 24 on `PATH`. `sprite` is a
second name for the same uid and the same home, kept so a client written
against sprites.dev resolves.

`tty=True` runs the command on a pseudo-terminal: everything arrives on
`stdout`, stdin is forced on, `end_stdin()` sends EOT instead of closing
anything, and `resize(cols, rows)` becomes callable.

**A close with no exit frame is an error.** `wait()` raises rather than
returning 0. A socket that dropped means nobody knows what the command did.

The pipes are queues fed by a reader thread and a WebSocket cannot be paused,
so a stream nobody reads grows. Read it, or use the buffered `machines.exec`.

## Builds

```python
build = pilots.builds.create(tar_bytes)
for line in build:
    print(line.step, line.line)
```

Or, when only the outcome matters:

```python
rootfs_build_id = pilots.builds.create(tar_bytes).result()
```

hostd answers 200 before the build starts, so a ten-minute build is watchable
while it runs. That means the status code cannot be the verdict: the last line
is. `result()` reads it, and raises `BuildFailedError` both when that line
carries an error and when the stream ended with no verdict at all.

`builds.create(tar, deploy="svc_1")` asks the host to cut that service a
release from the image, on the build's verdict, exactly once; `build.release`
carries the id afterwards. `builds.create_from_repo({"repo": "you/shop", "ref":
"main"})` builds a repository by naming it, through the fleet's GitHub App.

## `pilots.sprites_compat`

A sprites-shaped face over the same client, so a codebase written against
`sprites-py` moves by changing one import line.

```python
from pilots.sprites_compat import SpritesClient

client = SpritesClient(token=os.environ["PILOT_API_KEY"])
sprite = client.create_sprite("demo")
result = sprite.run("echo", "hello", capture_output=True)
print(result.stdout.decode())
sprite.destroy()
```

The rules that decide the shapes: a sprite's `id` is the machine's NAME (what a
sprites consumer persists and hands back), `machine_id` carries the `m-…` id;
`restore_checkpoint` restores in place and creates nothing, because a URL is
permanent; `run` and `command` mirror `subprocess.run` and `exec.Cmd` the way
`sprites-py` spells them. Services, network policy and the `/control`
multiplex are not here.

| sprites-py | pilots | Note |
| --- | --- | --- |
| `SpritesClient(token=…)` | `SpritesClient(token=…)` | reads `PILOT_API_KEY` when omitted |
| `client.create_sprite(name)` | same | `url_settings`, `labels` accepted; `wait_for_capacity` ignored |
| `client.sprite(name)` / `get_sprite` / `list_sprites` / `destroy_sprite` | same | |
| `sprite.run(*argv, capture_output, timeout, cwd, env)` | same | returns `subprocess.CompletedProcess` |
| `sprite.command(*argv).output()` / `.combined_output()` / `.run()` | same | |
| `sprite.create_checkpoint(comment)` / `list_checkpoints()` / `restore_checkpoint(id)` | same | in place |
| `sprite.filesystem()` | `sprite.filesystem()` | `read_text`, `write_text`, `listdir` over exec |
| `SPRITES_TOKEN` | `PILOT_API_KEY` | |

## Framework adapters

Each adapter is one module of this package that imports its framework lazily;
install the extra you use.

**Google ADK** (`pip install 'pilots-sdk[adk]'`):

```python
from google.adk.agents import Agent
from pilots.adk import PilotsPlugin

plugin = PilotsPlugin()  # or PilotsPlugin(machine_name="my-project") for a persistent one
root_agent = Agent(model="gemini-flash-latest", name="pilot_agent",
                   instruction="Run code and commands in the pilots machine, not locally.",
                   tools=plugin.get_tools())
```

Seven tools: `execute_command_in_machine`, `execute_code_in_machine`,
`write_file_to_machine`, `read_file_from_machine`, `create_machine_checkpoint`,
`list_machine_checkpoints`, `restore_machine_checkpoint` (destructive, requires
`confirm=True`). The machine is created lazily on first use; an unnamed one is
destroyed on `plugin.close()`, a named one is kept.

**OpenAI Agents SDK** (`pip install 'pilots-sdk[openai-agents]'`):

```python
from agents.run import RunConfig
from agents.sandbox import SandboxAgent, SandboxRunConfig
from pilots.openai_agents import PilotsSandboxClient

sandbox = await PilotsSandboxClient().create()
agent = SandboxAgent(name="pilots assistant", instructions="Inspect the workspace and do the task.")
run_config = RunConfig(sandbox=SandboxRunConfig(session=sandbox))
```

An ephemeral machine by default, destroyed when the session is cleaned up;
`PilotsSandboxClientOptions(machine_name="…")` attaches to an existing one.

**Claude Managed Agents** (`pip install 'pilots-sdk[anthropic]'`): Anthropic
runs the agent loop; every tool call executes inside a machine you own.

```python
from pilots.managed_agents import run_worker

asyncio.run(run_worker(environment_id=os.environ["ANTHROPIC_ENVIRONMENT_ID"],
                       environment_key=os.environ["ANTHROPIC_ENVIRONMENT_KEY"]))
```

Per work item the worker creates a machine, runs Anthropic's tool runner
(`ant beta:worker run`) inside it with the session's `ANTHROPIC_*` environment,
and destroys the machine when the session ends; `machine_name` keeps one
machine across sessions instead.

## Development

```
cd sdks/python
uv run --extra dev pytest
uv run --extra dev ruff check .
uv run --extra dev mypy src
```

The tests need no fleet: they run against an in-process fake hostd and a fake
websocket that speaks the same frames.
