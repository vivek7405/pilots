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
3. It suspends when idle and wakes on the next request. A sandbox nobody is using costs nothing.

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
