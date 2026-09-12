defmodule Pilots do
  @moduledoc """
  The Elixir client for pilots: instant sandboxes and durable production
  services on one primitive, Firecracker microVMs.

      client = Pilots.new()
      {:ok, machine} = Pilots.create_machine(client, name: "demo")
      machine["url"]  # => "https://demo.pilotrun.app"

      {:ok, %{"stdout" => out}} = Pilots.exec(client, machine["id"], "uname -a")

  One function per route, grouped by the noun it acts on, and every one
  answers `{:ok, value}` or `{:error, %Pilots.Error{}}`. The bang variants
  raise instead, for a caller who would rather let it crash.

  Responses are plain maps with the server's own keys rather than structs.
  That is deliberate: hostd adds fields, and a struct would either drop the
  new ones silently or force this package to be released before a consumer
  could see them. `test/drift_test.exs` reads hostd's Go source and fails when
  a documented field is missing from the module docs here, so the map's shape
  is still checked.

  Every host serves the identical API, so the base URL is any host in the
  fleet: there is no control-plane tier to be down, and a write that arrives
  at the wrong host is forwarded by hostd itself.
  """

  alias Pilots.{Error, HTTP}

  @typedoc "A client, from `new/2`."
  @type client :: HTTP.t()
  @type result :: {:ok, term()} | {:error, Error.t()}

  @doc """
  Builds a client.

  Options: `:base_url`, `:timeout` (milliseconds, JSON calls only), `:org` (an
  admin key acting as one org), and `:request_fun` (the transport seam).
  """
  @spec new(String.t() | nil, keyword()) :: client()
  defdelegate new(api_key \\ nil, opts \\ []), to: HTTP

  # -- the fleet ---------------------------------------------------------------

  @doc "Liveness. The one route that needs no key."
  @spec health(client()) :: result()
  def health(client), do: HTTP.request(client, :get, "/v1/health")

  @doc "The org, scopes and host this client's key resolves to."
  @spec whoami(client()) :: result()
  def whoami(client), do: HTTP.request(client, :get, "/v1/whoami")

  @doc "The fleet as the answering host sees it, from its local replica."
  @spec list_hosts(client()) :: result()
  def list_hosts(client), do: HTTP.request(client, :get, "/v1/hosts")

  # -- machines ----------------------------------------------------------------

  @doc """
  Creates a machine.

  No client deadline: a create from a template is sub-second, but a create
  from a build is a kernel boot, and an abort would leave a machine running
  that the caller has no id for.
  """
  @spec create_machine(client(), keyword() | map()) :: result()
  def create_machine(client, attrs \\ []) do
    HTTP.request(client, :post, "/v1/machines", body: to_body(attrs), timeout: :infinity)
  end

  @spec list_machines(client()) :: result()
  def list_machines(client), do: HTTP.request(client, :get, "/v1/machines")

  @spec get_machine(client(), String.t()) :: result()
  def get_machine(client, id), do: HTTP.request(client, :get, "/v1/machines/#{seg(id)}")

  @spec destroy_machine(client(), String.t()) :: result()
  def destroy_machine(client, id), do: HTTP.request(client, :delete, "/v1/machines/#{seg(id)}")

  @doc """
  Runs a command and waits for it.

  A non-zero exit is a RESULT, not an error: the call answers `{:ok, %{
  "exit_code" => 2}}` and the caller decides what that means. Only a refusal
  by the platform is an `{:error, _}`.
  """
  @spec exec(client(), String.t(), String.t(), keyword()) :: result()
  def exec(client, id, cmd, opts \\ []) do
    body = Enum.into(opts, %{cmd: cmd}) |> to_body()
    # A server-side timeout longer than the client's own extends it, with a
    # margin, so the server's timeout result arrives rather than a client abort.
    timeout =
      case Keyword.get(opts, :timeout_ms) do
        nil -> client.timeout
        ms when ms + 5_000 > client.timeout -> ms + 5_000
        _ -> client.timeout
      end

    HTTP.request(client, :post, "/v1/machines/#{seg(id)}/exec", body: body, timeout: timeout)
  end

  @doc "The machine's console log."
  @spec logs(client(), String.t()) :: {:ok, String.t()} | {:error, Error.t()}
  def logs(client, id), do: HTTP.request_raw(client, :get, "/v1/machines/#{seg(id)}/logs")

  @spec suspend(client(), String.t()) :: result()
  def suspend(client, id), do: HTTP.request(client, :post, "/v1/machines/#{seg(id)}/suspend")

  @spec wake(client(), String.t()) :: result()
  def wake(client, id), do: HTTP.request(client, :post, "/v1/machines/#{seg(id)}/wake")

  @doc """
  Boots a machine again at a new size, in place: same id, same URL, same disk,
  same volume.

  A boot rather than a resume, because a memory image cannot be loaded into a
  differently-sized VM, so the machine loses what was in memory. Omit a
  dimension to leave it as it is.
  """
  @spec resize(client(), String.t(), keyword()) :: result()
  def resize(client, id, opts \\ []) do
    HTTP.request(client, :post, "/v1/machines/#{seg(id)}/resize",
      body: to_body(vcpus: opts[:vcpus], mem_mib: opts[:mem_mib])
    )
  end

  @doc "Captures the machine, memory included, so it can be restored exactly."
  @spec checkpoint(client(), String.t(), String.t() | nil) :: result()
  def checkpoint(client, id, comment \\ nil) do
    HTTP.request(client, :post, "/v1/machines/#{seg(id)}/checkpoints",
      body: to_body(comment: comment)
    )
  end

  @spec list_checkpoints(client(), String.t()) :: result()
  def list_checkpoints(client, id),
    do: HTTP.request(client, :get, "/v1/machines/#{seg(id)}/checkpoints")

  @doc """
  Restores a checkpoint IN PLACE.

  The machine keeps its id, its URL and its agent token, and nothing new is
  created: a restore that created a machine would mint a new URL, and a URL is
  permanent.
  """
  @spec restore(client(), String.t()) :: result()
  def restore(client, checkpoint_id),
    do: HTTP.request(client, :post, "/v1/checkpoints/#{seg(checkpoint_id)}/restore")

  @doc "Turns a sandbox into a durable service. The URL does not change."
  @spec promote(client(), String.t(), keyword() | map()) :: result()
  def promote(client, id, attrs \\ []) do
    HTTP.request(client, :post, "/v1/machines/#{seg(id)}/promote", body: to_body(attrs))
  end

  # -- services ----------------------------------------------------------------

  @spec list_services(client()) :: result()
  def list_services(client), do: HTTP.request(client, :get, "/v1/services")

  @spec get_service(client(), String.t()) :: result()
  def get_service(client, id), do: HTTP.request(client, :get, "/v1/services/#{seg(id)}")

  @spec create_service(client(), keyword() | map()) :: result()
  def create_service(client, attrs),
    do: HTTP.request(client, :post, "/v1/services", body: to_body(attrs))

  @doc """
  Deploys a release.

  No client deadline, for the reason a build has none: a rollout takes as long
  as the release takes to prove itself, and an abort mid-rollout cancels the
  health gate and the cleanup that runs when it fails.
  """
  @spec deploy(client(), String.t(), keyword() | map()) :: result()
  def deploy(client, id, attrs \\ []) do
    HTTP.request(client, :post, "/v1/services/#{seg(id)}/deploy",
      body: to_body(attrs),
      timeout: :infinity
    )
  end

  @spec rollback(client(), String.t()) :: result()
  def rollback(client, id),
    do: HTTP.request(client, :post, "/v1/services/#{seg(id)}/rollback", timeout: :infinity)

  @spec releases(client(), String.t()) :: result()
  def releases(client, id), do: HTTP.request(client, :get, "/v1/services/#{seg(id)}/releases")

  @spec update_service(client(), String.t(), keyword() | map()) :: result()
  def update_service(client, id, attrs),
    do: HTTP.request(client, :patch, "/v1/services/#{seg(id)}", body: to_body(attrs))

  # -- builds ------------------------------------------------------------------

  @doc """
  Builds a context tar and returns every log line.

  `POST /v1/builds` answers 200 before the build starts, so the status code
  cannot be the verdict: the LAST line is. `build_result/1` reads it.
  """
  @spec build(client(), iodata(), keyword()) :: {:ok, [map() | String.t()]} | {:error, Error.t()}
  def build(client, tar, opts \\ []) do
    query = Keyword.take(opts, [:deploy, :app])

    with {:ok, body} <-
           HTTP.request_raw(client, :post, "/v1/builds",
             body: {"application/x-tar", tar},
             query: query,
             timeout: :infinity
           ) do
      {:ok, HTTP.ndjson(body)}
    end
  end

  @doc """
  The rootfs build id a build produced, or the failure it ended on.

  Both an `error` line and a stream that ended with no verdict at all are
  failures: an interrupted build must never read as a successful one.
  """
  @spec build_result([map() | String.t()]) :: {:ok, String.t()} | {:error, Error.t()}
  def build_result(lines) do
    case List.last(lines) do
      %{"error" => error} = line when is_binary(error) ->
        {:error,
         %Error{
           message: error,
           status: 200,
           body: Jason.encode!(line),
           code: Map.get(line, "code", "build_failed"),
           next: Map.get(line, "next", ""),
           details: nil
         }}

      %{"result" => result} when is_binary(result) ->
        {:ok, result}

      _ ->
        {:error, Error.transport("the build stream ended without a verdict")}
    end
  end

  @spec build_logs(client(), String.t()) :: {:ok, [map() | String.t()]} | {:error, Error.t()}
  def build_logs(client, id) do
    with {:ok, body} <-
           HTTP.request_raw(client, :get, "/v1/builds/#{seg(id)}/logs", timeout: :infinity) do
      {:ok, HTTP.ndjson(body)}
    end
  end

  # -- the rest ----------------------------------------------------------------

  @spec list_volumes(client()) :: result()
  def list_volumes(client), do: HTTP.request(client, :get, "/v1/volumes")

  @spec list_domains(client()) :: result()
  def list_domains(client), do: HTTP.request(client, :get, "/v1/domains")

  @doc """
  Asks the host what a directory is, from a tar of it.

  The front door: what a caller reaches for before it knows whether the
  directory is a compose project, a Dockerfile or a framework hostd
  recognises.
  """
  @spec plan(client(), iodata(), keyword()) :: result()
  def plan(client, tar, opts \\ []) do
    HTTP.request(client, :post, "/v1/plan",
      body: {"application/x-tar", tar},
      query: Keyword.take(opts, [:app]),
      timeout: :infinity
    )
  end

  @doc "Asks the host what a REPOSITORY is, naming it rather than sending it."
  @spec plan_repo(client(), String.t(), String.t(), keyword()) :: result()
  def plan_repo(client, repo, ref, opts \\ []) do
    HTTP.request(client, :post, "/v1/plan",
      body: %{repo: repo, ref: ref},
      query: Keyword.take(opts, [:app]),
      timeout: :infinity
    )
  end

  # -- bang variants -----------------------------------------------------------

  @doc "`create_machine/2`, raising on failure."
  @spec create_machine!(client(), keyword() | map()) :: term()
  def create_machine!(client, attrs \\ []), do: unwrap(create_machine(client, attrs))

  @doc "`exec/4`, raising on failure. A non-zero exit still returns normally."
  @spec exec!(client(), String.t(), String.t(), keyword()) :: term()
  def exec!(client, id, cmd, opts \\ []), do: unwrap(exec(client, id, cmd, opts))

  @doc "`list_machines/1`, raising on failure."
  @spec list_machines!(client()) :: term()
  def list_machines!(client), do: unwrap(list_machines(client))

  defp unwrap({:ok, value}), do: value
  defp unwrap({:error, %Error{} = error}), do: raise(error)

  # -- helpers -----------------------------------------------------------------

  @doc false
  def seg(value), do: URI.encode_www_form(to_string(value))

  # A nil is DROPPED rather than sent: hostd's structs carry `omitempty`, and
  # a null where a field was simply not set means something different there.
  defp to_body(attrs) when is_list(attrs), do: attrs |> Enum.into(%{}) |> to_body()

  defp to_body(attrs) when is_map(attrs) do
    attrs
    |> Enum.reject(fn {_k, v} -> is_nil(v) end)
    |> Enum.into(%{})
  end
end
