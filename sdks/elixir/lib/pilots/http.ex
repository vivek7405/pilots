defmodule Pilots.HTTP do
  @moduledoc """
  The transport: one bearer-authenticated `:httpc` call, one status-to-error
  map, and a line reader for the NDJSON a build streams.

  `:httpc` rather than a client library, because it ships with OTP. A caller
  who wants retries, pooling or tracing supplies their own `:request_fun`;
  adding any of them inside this module would be a dependency bought for
  convenience, which the platform's own rule forbids.
  """

  alias Pilots.Error

  @default_base_url "https://api.pilotrun.app"
  @default_timeout 30_000

  @typedoc "A client: the key, where to send it, and how long to wait."
  @type t :: %__MODULE__{
          api_key: String.t(),
          base_url: String.t(),
          timeout: timeout(),
          org: String.t() | nil,
          request_fun: (map() -> {:ok, map()} | {:error, term()}) | nil
        }

  defstruct [:api_key, :base_url, :timeout, :org, :request_fun]

  @doc """
  Builds a client.

  The key comes from the argument, then `PILOT_API_KEY`. The base URL comes
  from `:base_url`, then `PILOT_API_URL`, then the public fleet. Every host
  serves the identical API, so any host is a valid endpoint.

  Raises on an empty key rather than at the first call: a client built with no
  key would otherwise fail once per call with a 401 that says nothing about
  the cause.
  """
  @spec new(String.t() | nil, keyword()) :: t()
  def new(api_key \\ nil, opts \\ []) do
    key = api_key || System.get_env("PILOT_API_KEY") || ""

    if key == "" do
      raise ArgumentError,
            "an API key is required: Pilots.new(System.get_env(\"PILOT_API_KEY\"))"
    end

    base =
      (Keyword.get(opts, :base_url) || System.get_env("PILOT_API_URL") || @default_base_url)
      |> String.trim_trailing("/")

    %__MODULE__{
      api_key: key,
      base_url: base,
      timeout: Keyword.get(opts, :timeout, @default_timeout),
      org: Keyword.get(opts, :org),
      request_fun: Keyword.get(opts, :request_fun)
    }
  end

  @doc "The URL for a path, with the org narrowing and any query applied."
  @spec url(t(), String.t(), keyword()) :: String.t()
  def url(%__MODULE__{} = client, path, query \\ []) do
    params =
      query
      |> Enum.reject(fn {_k, v} -> is_nil(v) end)
      # The org narrowing is applied HERE rather than at each call site
      # because it has to reach every route: a client acting as an org must
      # create as it, be charged as it and read as it.
      |> then(fn q -> if client.org, do: q ++ [org: client.org], else: q end)
      |> Enum.map(fn {k, v} -> "#{k}=#{URI.encode_www_form(to_string(v))}" end)

    case params do
      [] -> client.base_url <> path
      _ -> client.base_url <> path <> "?" <> Enum.join(params, "&")
    end
  end

  @doc """
  Performs a request and decodes the JSON body.

  `:timeout` of `:infinity` disables the deadline, which is what a build, a
  log follow and a deploy use: they outlive any reasonable number.
  """
  @spec request(t(), atom(), String.t(), keyword()) ::
          {:ok, term()} | {:error, Error.t()}
  def request(%__MODULE__{} = client, method, path, opts \\ []) do
    with {:ok, status, body} <- send_request(client, method, path, opts) do
      cond do
        status >= 400 ->
          {:error, Error.from_response(status, body, "#{method} #{path}")}

        body == "" ->
          {:ok, nil}

        true ->
          case Jason.decode(body) do
            {:ok, decoded} -> {:ok, decoded}
            {:error, _} -> {:ok, body}
          end
      end
    end
  end

  @doc "Performs a request and returns the raw body, for a text route."
  @spec request_raw(t(), atom(), String.t(), keyword()) ::
          {:ok, String.t()} | {:error, Error.t()}
  def request_raw(%__MODULE__{} = client, method, path, opts \\ []) do
    with {:ok, status, body} <- send_request(client, method, path, opts) do
      if status >= 400 do
        {:error, Error.from_response(status, body, "#{method} #{path}")}
      else
        {:ok, body}
      end
    end
  end

  defp send_request(client, method, path, opts) do
    target = url(client, path, Keyword.get(opts, :query, []))
    headers = [{~c"authorization", String.to_charlist("Bearer " <> client.api_key)}]
    timeout = Keyword.get(opts, :timeout, client.timeout)

    request =
      case Keyword.get(opts, :body) do
        nil ->
          {String.to_charlist(target), headers}

        {content_type, raw} ->
          {String.to_charlist(target), headers, String.to_charlist(content_type),
           IO.iodata_to_binary(raw)}

        map ->
          {String.to_charlist(target), headers, ~c"application/json", Jason.encode!(map)}
      end

    do_request(client, method, request, timeout)
  end

  defp do_request(%{request_fun: fun} = _client, method, request, timeout)
       when is_function(fun, 1) do
    # The seam a test drives, and the seam a caller adds retries at.
    fun.(%{method: method, request: request, timeout: timeout})
  end

  defp do_request(_client, method, request, timeout) do
    http_options = [
      timeout: timeout,
      connect_timeout: 10_000,
      # Certificate verification is ON. `:httpc` defaults to verify_none,
      # which would make every https call here trust any certificate, and a
      # client that ships a bearer token over such a connection is worse than
      # no client at all.
      ssl: [
        verify: :verify_peer,
        cacerts: :public_key.cacerts_get(),
        depth: 3,
        customize_hostname_check: [
          match_fun: :public_key.pkix_verify_hostname_match_fun(:https)
        ]
      ]
    ]

    case :httpc.request(method, request, http_options, body_format: :binary) do
      {:ok, {{_version, status, _reason}, _headers, body}} ->
        {:ok, status, body}

      {:error, reason} ->
        {:error, Error.transport("#{method}: #{inspect(reason)}")}
    end
  end

  @doc """
  Splits an NDJSON body into decoded lines.

  A blank line is dropped and a line that does not parse is kept as a string,
  because a build log that ends mid-line should still show what arrived.
  """
  @spec ndjson(String.t()) :: [map() | String.t()]
  def ndjson(body) do
    body
    |> String.split("\n", trim: true)
    |> Enum.map(fn line ->
      case Jason.decode(line) do
        {:ok, decoded} -> decoded
        {:error, _} -> line
      end
    end)
  end
end
