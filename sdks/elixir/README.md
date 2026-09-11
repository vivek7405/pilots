# `pilots`

The Elixir client for the pilots API: instant sandboxes and durable production
services on one primitive, Firecracker microVMs.

One dependency, `jason`. Everything else is OTP's own: the HTTP calls go
through `:httpc`, so adding this to a project pulls in nothing that is not
already on the machine.

```elixir
def deps do
  [{:pilots, "~> 0.1"}]
end
```

## Using it

```elixir
client = Pilots.new()                       # PILOT_API_KEY, PILOT_API_URL

{:ok, machine} = Pilots.create_machine(client, name: "demo")
machine["url"]                              # => "https://demo.pilotrun.app"

{:ok, %{"stdout" => out}} = Pilots.exec(client, machine["id"], "uname -a")
{:ok, checkpoint} = Pilots.checkpoint(client, machine["id"], "before the risky bit")
{:ok, _} = Pilots.restore(client, checkpoint["id"])
:ok = Pilots.destroy_machine(client, machine["id"]) |> then(fn {:ok, _} -> :ok end)
```

Every function answers `{:ok, value}` or `{:error, %Pilots.Error{}}`. The bang
variants (`create_machine!`, `exec!`, `list_machines!`) raise the same struct
for a caller who would rather let it crash.

| Option to `new/2` | Default | Notes |
| --- | --- | --- |
| the key argument | `PILOT_API_KEY` | An empty key raises before any request is made. |
| `:base_url` | `PILOT_API_URL`, then `https://api.pilotrun.app` | Any host in the fleet is a valid endpoint. |
| `:timeout` | `30_000` | Milliseconds, JSON calls only. Builds, deploys and plans get none. |
| `:org` | none | Makes an ADMIN key act as one org. Every request carries `?org=`. |
| `:request_fun` | `:httpc` | The transport seam, for retries, pooling or tracing. |

## What it covers

| Call | Route |
| --- | --- |
| `health/1`, `whoami/1`, `list_hosts/1` | `/v1/health`, `/v1/whoami`, `/v1/hosts` |
| `create_machine/2` `list_machines/1` `get_machine/2` `destroy_machine/2` | `/v1/machines` |
| `exec/4`, `logs/2` | `/v1/machines/{id}/exec`, `/logs` |
| `suspend/2`, `wake/2` | `/v1/machines/{id}/suspend`, `/wake` |
| `checkpoint/3`, `list_checkpoints/2`, `restore/2` | checkpoints, and an in-place restore |
| `promote/3` | `/v1/machines/{id}/promote` |
| `list_services/1` `get_service/2` `create_service/2` `update_service/3` | `/v1/services` |
| `deploy/3`, `rollback/2`, `releases/2` | `/v1/services/{id}/…` |
| `build/3`, `build_result/1`, `build_logs/2` | `/v1/builds` |
| `plan/3`, `plan_repo/4` | `/v1/plan` |
| `list_volumes/1`, `list_domains/1` | `/v1/volumes`, `/v1/domains` |

Responses are plain maps with the server's own keys rather than structs, which
is a deliberate difference from the other three clients. hostd adds fields, and
a struct would either drop the new ones silently or force a release of this
package before a consumer could see them.

What that would otherwise cost is a check, so `test/drift_test.exs` parses
hostd's own route table and fails when a path this client calls is not one the
server registers. A renamed route is caught here rather than at runtime against
a real fleet.

## Three behaviours worth knowing

**A non-zero exit is a result, not an error.** `Pilots.exec/4` answers
`{:ok, %{"exit_code" => 2}}` for a command that failed, because the command
failing is what you asked to find out. Only a refusal by the platform is an
`{:error, _}`.

**A build's verdict is its last line.** `POST /v1/builds` answers 200 before
the build starts, so a ten-minute build is watchable while it runs and the
status code cannot be the verdict. `build/3` returns every line and
`build_result/1` reads the last one, raising a failure both when that line
carries an error and when the stream ended with no verdict at all.

**A restore is in place.** `restore/2` performs exactly one request and creates
nothing: the machine keeps its id, its URL and its agent token. A restore that
created a machine would mint a new URL, and a URL is permanent.

## Errors

```elixir
case Pilots.get_machine(client, "nope") do
  {:ok, machine} -> machine
  {:error, %Pilots.Error{code: "not_found", next: next}} -> IO.puts(next)
  {:error, error} -> IO.puts(Exception.message(error))
end
```

`code`, `next` and `details` come from the server's body and are on every
error, not only the ones this version has a name for, so a code this client has
never heard of still reaches you with its next step attached.

## Development

```sh
cd sdks/elixir
mix deps.get
mix test
mix format --check-formatted
```

The tests need no fleet: they drive the same `:request_fun` seam a caller would
add retries at.
