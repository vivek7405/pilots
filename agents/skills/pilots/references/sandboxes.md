# Sandboxes

## What this covers

A throwaway environment to run commands in: creating one, running commands, reading its console, capturing and restoring it. Not covered: turning one into a durable service (promote.md), deploying an app (deploy.md).

## The command to copy

    pilot machines create --name scratch
    pilot machines exec scratch -- ls /

MCP: `create_machine` with `{ "name": "scratch" }`, then `exec` with `{ "machine": "scratch", "cmd": "ls /" }`.

## What happens

1. A create is a RESTORE from a template, not a boot, so it is fast and the number is independent of the machine's size.
2. The machine gets a permanent URL derived from its name. The URL survives suspend, wake, checkpoint, restore and promote.
3. It suspends after 60 seconds of quiet (`idle_timeout`, up to an hour) and wakes on the next request or exec. A sandbox nobody is using costs nothing. Quiet means no request, no exec, and no console session running a command; see "Background work" below.

## The tools

| I need to... | Tool | Note |
| --- | --- | --- |
| a machine | `create_machine` | `name` is optional; pass one when the URL has to be predictable |
| run a command and read the output | `exec` | waits; returns stdout, stderr and the exit code |
| run something long or noisy | `exec_stream` | collects output as it arrives |
| read the console | `logs` | `tail` limits it to the last lines |
| capture it, memory included | `checkpoint` | returns a checkpoint id |
| put a checkpoint back | `restore` | IN PLACE: same machine, same id, same URL |
| get rid of it | `destroy_machine` | irreversible, and it takes the checkpoints with it |

## Each answer and what to do

| Answer | Meaning | Do |
| --- | --- | --- |
| a non-zero `exit_code` from `exec` | the command ran and failed | decide what it means; this is a result, not a tool error |
| `not_found` | the machine is gone, or this key sees a different org | check the id; `list_machines` shows what the key can see |
| `quota_exceeded`, 429 | the org is at its machine, vCPU or memory ceiling | `next` names the limit; destroy something or ask an admin |

## Do not

- Do not create a machine to deploy an app. `deploy` makes the machines a service needs.
- Do not treat a non-zero exit as a broken tool. A `grep` that found nothing exits 1.
- Do not destroy a machine without asking the user. It is irreversible and it takes every checkpoint with it.

## Files, ports and sessions from the CLI

- `pilot file push <local> <machine>:<dest>` / `pilot file pull <machine>:<src> <local>` /
  `pilot file edit <machine>:<path>` move files over the exec stream; a binary file
  survives the round trip byte for byte.
- `pilot proxy <port>|<local:remote>...` reaches any TCP port inside a machine from
  localhost (Postgres, a debugger, ssh via `-W host:port` as a ProxyCommand). The URL
  serves 8080 over HTTP only; proxy is for everything else.
- A console is a session that outlives its connection: `ctrl-\` detaches, `pilot attach`
  returns and replays what was printed meanwhile, `pilot sessions ls` lists them,
  `pilot sessions kill` ends one.
- `pilot url [target]` shows a URL's auth mode; `pilot url update --auth org` makes it
  require an API key of the org; `--label k=v` on create and `ls --label` find machines again.
- `pilot use <machine>` sets a directory-local default so none of these need a name.

## Background work

A suspend is a freeze, not a kill: the memory is snapshotted and every process resumes exactly where it was on the next request, exec or attach. So the question is only what keeps a machine awake, and there are three signals:

- A request or exec in flight, for its whole life. A long silent build under `exec` is never suspended.
- A console session with a command running, even after you detach. The guest reads its process tree, so a build whose output goes to a file still counts; a shell sitting at its prompt does not. `pilot sessions ls` shows `busy` per session.
- Guest-to-guest traffic on `.internal`, and open sessions between machines.

What none of them see is a process nothing is connected to: a daemon you started with `setsid`, or a worker that polls an outside queue. For that, set how long the machine waits after its last activity:

    pilot machines create worker --idle-timeout 30m

MCP: `create_machine` with `{ "idle_timeout": 1800 }`. The cap is an hour, so a forgotten value costs at most an hour per idle cycle. A worker that must run forever is a service, not a sandbox: `promote` it, then `x-pilots: min_machines_running: 1` in its compose file keeps one replica resident.

Two things to know: a process that calls `setsid` leaves the session tree by definition, which is why the timeout exists; and a long sleep can drop outbound connections, so a client that resumes after an hour should expect to reconnect.

## What a machine is using

| I need to... | Do |
| --- | --- |
| one machine's CPU and memory | `metrics` tool, or `pilot machines metrics <m>` |
| every machine of a team | `GET /v1/metrics`, Prometheus text, with a `machines` key |
| the end of a log, not the boot | `pilot machines logs <m> --tail 50` |
| resume a follow that dropped | `?offset=` with the `X-Pilot-Log-Offset` you last saw |

CPU is a TOTAL in seconds, not a rate. Take two readings and divide by the time
between them. It never goes down, including across a suspend, so a difference is
always real work.

Memory is zero while a machine is suspended. That is the truth rather than a
gap: a suspended machine holds no memory, which is the point of suspending it.

Memory near the ceiling is why a process was killed. CPU flat while a request
hangs means it is waiting on something, not computing.

```
# Prometheus, scraping one team's machines from any host:
scrape_configs:
  - job_name: pilots
    metrics_path: /v1/metrics
    authorization:
      credentials: <a machines-scoped key>
    static_configs:
      - targets: ['api.example.test']
```

A host that did not answer appears as `pilots_metrics_hosts_unreachable`. The
scrape is still a 200: what could be read is more useful than nothing, and the
line names what could not.

Console logs are bounded at 16 MiB per machine and are NOT state. Wipe a host
and the logs are gone while every machine restores exactly as before.
