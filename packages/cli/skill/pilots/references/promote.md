# Promote

## What this covers

Turning a sandbox into a durable service without changing its URL. Not covered: creating a sandbox (sandboxes.md), deploying from a directory (deploy.md).

## The command to copy

    pilot machines promote scratch

MCP: `promote` with `{ "machine": "scratch" }`.

## What happens

1. The machine becomes a service's first replica.
2. The URL does not change. That is the whole point: every link to the sandbox keeps working against the service.
3. The service gets replicas, a health gate and releases; the sandbox's lifecycle knobs are replaced by the service's.

## When to reach for it

| Situation | Promote? |
| --- | --- |
| a sandbox someone has been sharing a link to, now worth keeping | yes |
| an experiment that should become production without a new URL | yes |
| a directory of source you want deployed | no, use `deploy` |
| a machine created from the template with no image | no, it has nothing to redeploy from |

## Each answer and what to do

| Answer | Meaning | Do |
| --- | --- | --- |
| a service id and the same URL | promoted | report it; `service` confirms |
| `bad_request` about an image | the sandbox was created from the template, not from an image | recreate it with `image` and promote that |
| `bad_request` about replicas | the machine mounts a volume | a volume-backed service runs one replica |

## Do not

- Do not create a new service and copy files. The URL would change, and the links would break.
- Do not promote a machine you have not told the user about. It changes what the machine costs.
