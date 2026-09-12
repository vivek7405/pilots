# Errors

## What this covers

Every error body pilots returns, what each code means, and the one thing to do about it. Not covered: what to deploy in the first place (deploy.md).

## The shape

Every non-2xx body is the same four fields:

    {
      "error": "a sentence for a person",
      "code": "health_gate_failed",
      "next": "the one thing to do about it",
      "details": { }
    }

`code` is a closed list. Branch on it, never on the wording. `next` names the command or the call to make now. `details` is typed per code and absent otherwise. The CLI prints the error and then `next` on its own line; `--json` prints the server's body unchanged.

## Every code

| Code | Status | Meaning | The `next` shape |
| --- | --- | --- | --- |
| `bad_request` | 400 | the request is malformed or missing a field | which field to pass, or the request type to shape the body like |
| `unauthorized` | 401 | no key, or a key that does not resolve | `pilot login`, or set `PILOT_API_KEY` |
| `scope_required` | 403 | the key resolves but lacks the scope | mint a key with that scope |
| `not_found` | 404 | no such object, or this key sees a different org | check the id; `pilot whoami` shows the org |
| `conflict` | 409 | an operation is already running on it | retry once it finishes |
| `volume_in_use` | 409 | a volume is attached or mounted elsewhere | destroy that machine, or detach it from that service |
| `quota_exceeded` | 429 | the org is at a ceiling | which limit; free something or raise it |
| `not_configured` | 501, 503, 400 | this host or fleet was built without the piece | which environment variable or which host |
| `not_implemented` | 501 | the route is not built yet | nothing calls it |
| `unavailable` | 503 | the host that writes this object is unreachable | retry in a minute |
| `internal` | 500 | the host broke | retry; the cause is in `details.cause` and in the host's journal |
| `plan_unsupported` | 400 | the compose file asks for something pilots does not do | fix each key in `details.unsupported` |
| `compose_invalid` | 400 | the compose file does not parse | the message names the file |
| `unknown_framework` | 400 | no compose file, no Dockerfile, no recipe | write a Dockerfile from `details` |
| `plan_multi_service` | 400 | a push tried to deploy more than one service | commit a compose file |
| `build_failed` | on the log line | the build stopped | the line carrying `error` says where |
| `health_gate_failed` | 422 | the replica never answered its health check | `diagnose` with `details.replica` |

## The two that carry details worth reading

`health_gate_failed` carries `{service, replica, release, grace_sec, last}`. `last` is `{status, body}` when the replica answered and `{error}` when it did not. It carries NO address, because the probe target is the host's own view of the replica and is not reachable from where you are reading this. `last.error` naming a refused connection means one of two things and almost nothing else: the app is listening on the wrong port, or it bound `127.0.0.1` instead of `0.0.0.0`.

`unknown_framework` carries `{dir, looked_for, listing, manifests, workspaces, rules}`. That is enough to write a Dockerfile without opening the repository again: `listing` is what is there, `manifests` is what the project declares, and `rules` is the two lines the Dockerfile must obey.

## Each answer and what to do

| Answer | Do |
| --- | --- |
| any error | read `next` and do that |
| `health_gate_failed` | `diagnose` with `details.replica`, then fix the app; do not redeploy first |
| `build_failed` | read the log line carrying `error`, fix the Dockerfile, `build` again |
| `unknown_framework` | write the Dockerfile from `details`, `build` with it, `deploy` with `name` and `build` |
| `internal` | retry once; if it repeats, say so rather than working around it |
| a 503 on a machine whose owner is gone | retry. The host that would bring it back is still joining the fleet and claims nothing until its replica has caught up. `GET /v1/health` on that host says `replication_complete: false` while this is true, and it clears itself within seconds |

## Do not

- Do not parse the `error` sentence. It is for a person and it will be reworded.
- Do not retry a 422 by deploying again. Nothing changed, so nothing will.
- Do not treat a 404 as proof the object does not exist. A key scoped to another org gets the same answer, on purpose.
