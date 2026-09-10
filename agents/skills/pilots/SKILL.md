---
name: pilots
description: Deploy and operate apps on pilots, the sandbox + PaaS on Firecracker microVMs. Use whenever the user mentions pilots, deploying an app, a sandbox, a service, a URL for a repo, secrets, volumes, custom domains, promote, or a failed deploy, even without the word pilots.
---

# Use pilots

## What pilots is

One primitive: a machine. A sandbox and a production replica are the same machine with different lifecycle knobs. A service is one or more machines behind a permanent URL. Every host serves the whole API, so no call depends on a particular machine being alive. A deploy is a build, then a restore, then a health gate; the URL never changes.

## The one call

`pilot deploy` in the app's directory, or the MCP tool `deploy` with `dir`. The platform decides what the directory is: a compose file, a Dockerfile, or a framework it recognises. Do not write a Dockerfile or a compose file first. Write one only when the answer is `unknown_framework`.

## Preflight

`pilot whoami`, `pilot status`, and the MCP `init` tool when driving the MCP. Nothing else before a deploy.

## Reach for the right primitive

| I need to... | Reach for | Reflex to resist | Reference |
| --- | --- | --- | --- |
| get a URL for this directory | `deploy` with `dir` | writing a Dockerfile or compose file first | references/deploy.md |
| see what a deploy would do | `plan` | reading the repo to guess the framework | references/deploy.md |
| a throwaway environment to run commands in | `create_machine` then `exec` | deploying a service | references/sandboxes.md |
| keep a sandbox as a production thing | `promote` | creating a service and copying files | references/promote.md |
| a second service, a database, a queue | a compose file, then `deploy` | several `deploy` calls with copied ids | references/compose.md |
| a password or token in the environment | `secret://` in compose, `pilot secrets` | pasting the value into env | references/secrets.md |
| data that survives a redeploy | a volume in compose | writing into the rootfs | references/volumes.md |
| my own hostname | `pilot domains add` | a DNS record with no verification | references/domains.md |
| find out why the deploy failed | the error's `code`, `next`, `details`, then `diagnose` | re-running the deploy | references/errors.md |
| logs of a running service | `service` then `logs` on a replica | `exec` with tail | references/services.md |

## Load only the reference you need

| Task involves... | Start with |
| --- | --- |
| a deploy, a URL, a monorepo, a Dockerfile | references/deploy.md |
| a sandbox, exec, checkpoints | references/sandboxes.md |
| releases, rollback, replicas, health | references/services.md |
| secret values | references/secrets.md |
| a volume, Postgres | references/volumes.md |
| a custom domain | references/domains.md |
| promote | references/promote.md |
| any error body | references/errors.md |
| a compose file | references/compose.md |

One reference is usually enough; two at most.

## Execution rules

1. Prefer the MCP tools. The CLI with `--json` is the same surface.
2. Resolve the directory before anything else. No directory and no repository in the conversation means ask; never invent one.
3. Read `next` on every result and every error, and do that.
4. A destructive tool, `destroy_machine` or `rollback`, is confirmed with the user first.
5. After a mutation, read it back with `service` or `status`.
6. Never read source to deploy. The platform's answer is complete, or it says what is missing.

## User-only commands

| Command | Why |
| --- | --- |
| `pilot login` | it opens a browser and stores a credential |
| `pilot domains add` | it needs a DNS record the user creates |
| `pilot secrets set` | the value must not pass through the model |

## Response format

1. What was done. 2. The URL, the ids, the release. 3. What to do next, or that it is done.
