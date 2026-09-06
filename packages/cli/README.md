# `@pilots/cli`

The `pilot` command, and the MCP server it ships.

Two front doors to the same fleet. A person runs `pilot deploy`; an agent runs
`pilot mcp` and drives the same API over stdio JSON-RPC.

```
npm install -g @pilots/cli
pilot login
pilot deploy
```

Node 24 or newer. There is no build step: the CLI is TypeScript run directly by
Node's type stripper, so what npm installs is what runs.

## Authenticating

`pilot login` runs the GitHub device flow. It prints a URL and a code, waits
for you to enter them, and exchanges the resulting GitHub token for a pilots
API key at the dashboard. The key is written to
`${XDG_CONFIG_HOME:-~/.config}/pilots/credentials`, mode `0600`.

The CLI ships a `client_id` and no client secret. The device flow is the OAuth
grant designed for a client that cannot keep one.

```
pilot login                       # the device flow
pilot login --token pilot_xxx     # headless: no GitHub, no dashboard
pilot logout                      # remove the file
pilot whoami                      # the org, the fleet, the key prefix, and where each came from
```

With no GitHub App configured and a terminal to ask on, `pilot login` prompts
for an API key and never echoes it. Off a terminal it names the headless path
instead, since there is nobody to prompt.

**No command validates a cached key.** Once a key is in the file, every command
talks only to the fleet. That is deliberate: a CLI that checked its credential
against the dashboard would make every command depend on the dashboard being
up, which is the central dependency this platform is built without.

### Where the key and the fleet come from

| | API key | Fleet URL |
|---|---|---|
| highest | `PILOT_API_KEY` | `--api-url` |
| | the credentials file | `PILOT_API_URL` |
| | | the credentials file |
| lowest | | `https://api.pilotrun.app` |

`pilot whoami` prints the source that won for each row, so a file holding one
key while `PILOT_API_KEY` holds another is visible rather than guessed at. It
asks the fleet for the org and scopes, since the file records only what `login`
stored and says nothing about a key from the environment. When the fleet does
not answer it says so on stderr, falls back to the file, and still exits `0`.

Other variables the CLI reads:

- `PILOT_GITHUB_CLIENT_ID` — the GitHub App's public client id for `pilot login`.
- `PILOT_DASHBOARD_URL` — where the token exchange happens; defaults to `https://pilots.run`.
- `PILOT_GITHUB_URL` — the GitHub the device flow runs against; defaults to `https://github.com`. Point it at a GitHub Enterprise Server to use one.
- `PILOT_SECRET_<NAME>` — the value for a `secret://<name>` reference in a compose file; `pilot secrets` stores the same value in the credentials file instead.

### The credentials file

```json
{
  "api_key": "pilot_...",
  "api_url": "https://api.pilotrun.app",
  "org_id": "org_...",
  "secrets": { "shop": { "postgres_password": "...", "database_url": "..." } }
}
```

`secrets` holds values that `secret://` references in a compose file resolve to
on this machine. The directory is `0700` and the file is `0600`; a file that any
other user can read is refused with the path named, because a mode that drifted
through a backup restore or a dotfiles checkout is silent until it is not.

`pilot secrets set`, `import` and `ls` are the commands that write and list it;
`add postgres` writes its generated password there too.

## Output and exit codes

Every command takes the global `--json`. With it, stdout carries the API's own
response, two-space indented, and nothing else; without it, a short table.
Diagnostics, prompts and errors always go to stderr.

| Exit code | Means |
|---|---|
| `0` | success |
| `1` | any failure |
| the remote code | `pilot machines exec`, which exits with the command's own status |
| `130` | interrupted |

An error from the fleet is rendered twice over. Under `--json` the server's body
reaches stderr **unchanged**, so it can be compared byte for byte with what the
SDK and the MCP server report:

```
$ pilot --json machines create
{"error":"quota exceeded","quota":"machines","limit":20,"used":20}

$ pilot machines create
error: quota exceeded: machines (limit 20, used 20)
```

An error the CLI raises itself carries the next step on a second line:

```
$ pilot machines ls
error: no API key
→ run pilot login, or set PILOT_API_KEY
```

Hints are stderr prose. They never appear under `--json`, where stderr carries
the server's body and nothing else, and they are never folded into the message,
so the sentence and the fix stay separately readable and separately testable.

Colour, where there is any, is stderr only and on a terminal only. `NO_COLOR`
turns it off, and so does `--json`.

## Commands

### Machines

A machine is the one primitive: a sandbox and a production replica are the same
object with different lifecycle knobs. Every command takes an id or a name.

```
pilot machines create [--name --image --template --checkpoint --vcpus --mem-mib
                       --app --cmd --env K=V --volume]
pilot machines ls [--app <name>]
pilot machines info <machine>
pilot machines exec <machine> [--cwd --env K=V --user --stdin] -- <argv...>
pilot machines logs <machine> [--follow]
pilot machines checkpoint <machine> [--comment <text>]
pilot machines checkpoints <machine>
pilot machines restore <checkpoint-id>
pilot machines suspend|wake|start|stop|destroy <machine>
pilot machines volume <machine>
```

`exec` streams the command's output frame by frame, stdout to stdout and stderr
to stderr, and exits with the remote status. **stdin is off unless you pass
`--stdin`**: a guest process holding an open stdin it never reads hangs.

`restore` is in place. The machine keeps its id, its URL and its agent token, so
every link to it still works.

Every table a person reads leads with the **name** and ends with the **id**.
The name is what goes into the next command; the id is 42 characters and, in
column one, pushed the URL off the right edge of a normal terminal.

`start` and `stop` answer `501` on the current server. The CLI passes the
server's error through rather than hiding it.

### Deploying

```
pilot deploy [dir] [--app <name>] [--env K=V] [--file <path>]
                   [--no-wait | -d, --detach] [--verbose] [-c, --ci]
```

The compose file is looked for in this order, first hit wins:
`compose.yaml`, `compose.yml`, `docker-compose.yml`, `docker-compose.yaml`.

`[dir]` says where to look for that file. **Every build context resolves
against the compose file's own directory**, which is not `[dir]` when `--file`
points somewhere else: `pilot deploy . --file infra/compose.yaml` builds from
`infra/`.

By default the build prints one line per stage with the time that stage took,
and, on a failure, the failing step's own output. `--verbose` streams every
line the way it used to; `-c/--ci` is `--verbose` with no colour and no line
rewritten in place, and a truthy `CI` variable means the same thing. Under
`--json` stderr carries the build's NDJSON unchanged and nothing else.

`-d/--detach` is `--no-wait` under railway's name: each deploy is accepted and
the run returns without polling for the release, and the result table gains a
`RELEASE` column.

**The CLI does no interpolation.** It posts the file's text plus the `.env`
file's map to `POST /v1/compose/plan` and executes the ordered plan that comes
back. One compose parser, in Go, beside the daemon.

The interpolation environment is the `.env` file only, never `process.env`, so
a deploy is reproducible from the checkout. `--env K=V` adds to it for a one-off.
A `${VAR}` with no default and no value there is refused by name rather than
deployed blank.

The app name comes from `COMPOSE_PROJECT_NAME`, then a top-level `x-pilots.app`,
then a top-level `name:`. A file with none of the three is refused rather than
given a default, because the app is what groups the services that reach each
other over `<name>.internal`. `--app` renames the plan after it comes back, so a
nameless file needs `--env COMPOSE_PROJECT_NAME=<name>` instead.

For each step, in dependency order: build the context, create the volume it
declares (one per service; the service mounts it and runs one replica), run
`x-pilots.pre_deploy` as a one-shot machine, create or patch the service, deploy,
and wait for the new release to become current.

A volume is set when the service is created and never changed: nothing copies
data between two volumes, so a compose file that renames one is refused rather
than quietly deployed onto an empty disk.

A service gets a URL only when it has a domain, so the `URL` column is empty
until one is set with `x-pilots.domain` in the compose file or
`pilot domains add <host> --service <name>`. The deploy says so in place of the
empty cell. The replicas are reachable at their own machine URLs regardless.

A `secret://name` value in the compose file never travels as a value. hostd
returns the reference, the CLI resolves it from `PILOT_SECRET_<NAME>` or the
credentials file, and sends the result as `secret_env`, which hostd seals. Every
missing name is reported at once, before anything is built. Store values with
`pilot secrets`.

### Secrets

```
pilot secrets set <name> [value] [--app <app>] [--dir <path>] [--env K=V] [--file <path>]
pilot secrets import <file>      [--app <app>] [--dir <path>] [--env K=V] [--file <path>]
pilot secrets ls                 [--app <app>] [--dir <path>] [--env K=V] [--file <path>]
```

The app is `--app`, or the compose file's app by the same rule `deploy` uses:
`COMPOSE_PROJECT_NAME` from the directory's `.env`, then a top-level
`x-pilots.app`, then a top-level `name:`. A file with none of the three is
refused rather than given a default, because a value stored under the wrong name
is a value the deploy will not find. `--env K=V` and `--file <path>` are the
same flags `deploy` has and feed the same derivation, so a project deployed as
`deploy --env COMPOSE_PROJECT_NAME=prod` or `deploy --file prod.compose.yaml`
stores its secrets under the app that deploy resolves against.

`set` with no value prompts on a terminal and never echoes what is typed. When
stdin is not a terminal it reads stdin to EOF instead, so
`printf '%s' "$V" | pilot secrets set n` works in a script. One trailing newline
is stripped, since that one belongs to `echo` rather than to the secret.

A value that starts with a dash is stored as a value, not read as an option, so
`pilot secrets set tok -sk-live-...` and `pilot secrets set tok -- -sk-live-...`
both work. The cost of accepting an unknown option there is that a mistyped flag
becomes an argument instead: `set` takes at most two, so `--ap x` is refused for
arity rather than named. That refusal counts the arguments and never lists them,
because one of them is the secret.

`import` takes a `.env` file, or `-` for stdin, and stores every pair in it in
one command. It is parsed by Node's own `.env` parser, the same one `deploy`
uses for interpolation, so a value that quotes correctly for one quotes
correctly for the other. The key is the secret name, matched exactly: a file
that says `DATABASE_URL=` stores `DATABASE_URL`, which `secret://database_url`
does not resolve to. A deploy that misses a name the store holds in a different
case says so rather than reporting it as unset.

`ls` prints each name and the first eight hex characters of the SHA-256 of its
value. That is enough to tell two machines hold the same secret and not enough
to recover it; no command in this group prints a value, on success or on
failure.

None of the three talks to the fleet. A value has to be storable on a laptop
with no host in reach, so every subcommand is local file manipulation against
the `0600` credentials file.

`set` and `import` therefore need that file, and `PILOT_API_KEY` on its own is
not enough: it signs requests, and a secret is written rather than sent. Writing
the file from the variable would persist a key nobody asked to persist, or leave
one with no `api_key` in it. So they refuse and name `pilot login` for a laptop
and `PILOT_SECRET_<NAME>` for a CI runner, which is the right answer there
anyway, since a file the job throws away stores nothing.

`PILOT_SECRET_<NAME>` still wins over the file at deploy time, so a CI job
overrides a stored value without editing anything.

### Services, domains, volumes

```
pilot services ls [--app <name>] [--wide]
pilot services info <service>|releases <service>|rollback <service>
pilot services set <service> [--replicas --env K=V --unset-env KEY
                              --secret-env K=V --repo --branch --autodeploy]
pilot domains add <hostname> --service <id|name>
pilot domains ls
pilot domains rm <hostname>
pilot volumes create <name> --size-gib N [--mount-path /data]
pilot volumes ls
pilot promote <machine> [--custom-domain --replicas --health-path]
pilot status
pilot logs <service> [-f, --follow]
pilot open <target> [-p, --print]
pilot completion bash|zsh|fish
```

`logs` fans in every replica of a service and prefixes each line with the
replica's name. `machines logs` is still there for one machine. Under `--json`
without `--follow` it is one object; with `--follow` it is one object per line,
so an agent can read it as it arrives.

`open` resolves a service first and a machine second, and spawns the platform
opener with an argv array, never through a shell. With `-p`, with `--json`, or
when stdout is not a terminal it prints the URL instead.

### Shell completion

```
eval "$(pilot completion bash)"      # ~/.bashrc
pilot completion zsh  > "${fpath[1]}/_pilot"
pilot completion fish > ~/.config/fish/completions/pilot.fish
```

The script is generated from the command tree itself, so a new command
completes without a second list to update.

### Scripts and agents

The global `-y/--yes` answers every confirmation. `machines destroy` and
`domains rm` ask before acting, but only when there is somebody to answer:
never under `-y`, never under `--json`, and never when stdin or stderr is not a
terminal. A script and an agent are therefore never blocked on a question.

`services set --env` merges onto what the service already carries, because the
underlying `PATCH` replaces the whole map. `--secret-env` replaces the sealed
map outright: a sealed value cannot be read back, so there is nothing to merge
with.

`services ls --wide` adds the current release id, the one column with no
everyday use. `services info` always carries it, since it shows a single service
someone is already looking closely at.

`promote` turns a sandbox into a durable service **without changing its URL**.

### Adding a database

```
pilot add postgres [--durable-volume] [--name postgres] [--app <name>] [--dir .]
```

Appends a Postgres service to the compose file, keeping its comments, generates
a password, and prints it once. See [`docs/postgres.md`](../../docs/postgres.md)
for the two durability modes and the recovery procedure.

## `pilot mcp`

Runs the MCP server on stdio. Add it to any MCP client:

```json
{
  "mcpServers": {
    "pilots": {
      "command": "pilot",
      "args": ["mcp"],
      "env": { "PILOT_API_KEY": "pilot_..." }
    }
  }
}
```

Thirteen tools: `create_machine`, `list_machines`, `status`, `exec`,
`exec_stream`, `logs`, `checkpoint`, `restore`, `build`, `deploy`, `promote`,
`destroy_machine`, `generate_dockerfile`.

Three behaviours are worth knowing before writing an agent against them:

- **A non-zero exit from `exec` is a result, not an error.** A `grep` that found
  nothing is not a broken tool.
- **A failed `build` carries every NDJSON line.** Read the failing step, patch
  the Dockerfile, call `build` again with the new text in `dockerfile`. Nothing
  has to be written to disk.
- **An API refusal carries the server's own body.** A 429 arrives exactly as
  hostd wrote it, quota name, limit and usage included.

`generate_dockerfile` detects the framework in a directory and returns a
Dockerfile, a port and a health check: webjs, Next.js, React Router / Remix,
Vite, Django, FastAPI, Rails, Go, Rust and Laravel.

### The two Dockerfile rules

Any Dockerfile an agent writes itself must:

1. **bind `0.0.0.0`**, never `127.0.0.1`. A service bound to loopback serves
   only the guest itself, and the router's proxy into the netns reaches nothing.
2. **read the port from `$PORT`**, with the framework's default as a fallback.

Both mistakes produce a build that succeeds and a URL that answers 502, with
nothing in the build log to read. `127.0.0.1` is correct in exactly one place: a
`HEALTHCHECK` probe, which runs inside the guest.

## Developing

From the repo root:

```
npm install
npm run build:sdk-js     # @pilots/sdk publishes from dist/
npm test --workspace=packages/cli
```

`npm test` in this package runs `tsc --noEmit` and `node --test`. The type check
is not optional: `erasableSyntaxOnly` is what catches an `enum` or a parameter
property before Node's stripper rejects it at runtime.
