defmodule PilotsTest do
  @moduledoc """
  The client, against the transport seam rather than a network.

  `:request_fun` is the same seam a caller would add retries at, so driving it
  here tests exactly the path production takes: the URL that was built, the
  header that was set, the body that was encoded, and how an answer is
  classified.
  """
  use ExUnit.Case, async: true

  alias Pilots.Error

  setup do
    # Every request is recorded, and each test says what comes back.
    {:ok, agent} = Agent.start_link(fn -> %{calls: [], responses: []} end)

    respond = fn responses ->
      Agent.update(agent, &%{&1 | responses: responses})
    end

    fun = fn call ->
      Agent.update(agent, &%{&1 | calls: &1.calls ++ [call]})

      Agent.get_and_update(agent, fn state ->
        case state.responses do
          [head | rest] -> {head, %{state | responses: rest}}
          [] -> {{:ok, 200, "{}"}, state}
        end
      end)
    end

    calls = fn -> Agent.get(agent, & &1.calls) end
    {:ok, respond: respond, fun: fun, calls: calls}
  end

  defp client(fun, opts \\ []) do
    Pilots.new(
      "pilot_testkey",
      Keyword.merge([base_url: "http://fleet.test", request_fun: fun], opts)
    )
  end

  defp url_of(call), do: call.request |> elem(0) |> to_string()
  defp headers_of(call), do: call.request |> elem(1)

  defp body_of(call) do
    case call.request do
      {_u, _h, _t, body} -> Jason.decode!(body)
      _ -> nil
    end
  end

  test "an empty key is refused before any request is made" do
    assert_raise ArgumentError, ~r/API key is required/, fn ->
      Pilots.new("", base_url: "http://fleet.test")
    end
  end

  test "the base url comes from the argument, then the environment, then the fleet" do
    System.put_env("PILOT_API_URL", "http://from-env:8080/")
    assert Pilots.new("k").base_url == "http://from-env:8080"
    System.delete_env("PILOT_API_URL")
    assert Pilots.new("k").base_url == "https://api.pilotrun.app"
    assert Pilots.new("k", base_url: "http://explicit/").base_url == "http://explicit"
  end

  test "every call carries the bearer token, and the org narrowing reaches all of them",
       %{fun: fun, calls: calls} do
    c = client(fun, org: "acme")
    Pilots.list_machines(c)
    Pilots.list_services(c)

    for call <- calls.() do
      assert {~c"authorization", ~c"Bearer pilot_testkey"} in headers_of(call)
      assert String.contains?(url_of(call), "org=acme")
    end
  end

  test "a nil field is dropped rather than sent as null", %{fun: fun, calls: calls} do
    c = client(fun)
    Pilots.create_machine(c, name: "demo", vcpus: nil, mem_mib: 512)

    assert body_of(hd(calls.())) == %{"name" => "demo", "mem_mib" => 512}
  end

  test "a 2xx answers {:ok, decoded}", %{fun: fun, respond: respond} do
    respond.([{:ok, 201, ~s({"id":"m-1","name":"demo","url":"https://demo.test"})}])
    assert {:ok, machine} = Pilots.create_machine(client(fun), name: "demo")
    assert machine["id"] == "m-1"
    assert machine["url"] == "https://demo.test"
  end

  test "a failure carries the server's own code, next and details",
       %{fun: fun, respond: respond} do
    body = ~s({"error":"no such machine","code":"not_found","next":"pilot machines ls"})
    respond.([{:ok, 404, body}])

    assert {:error, %Error{} = error} = Pilots.get_machine(client(fun), "nope")
    assert error.status == 404
    assert error.code == "not_found"
    assert error.next == "pilot machines ls"
    assert error.message == "no such machine"
    # The next step is in the message a caller prints, because it is the
    # actionable half.
    assert Exception.message(error) =~ "next: pilot machines ls"
    assert Error.code?(error, "not_found")
  end

  test "a code this version has never heard of still reaches the caller",
       %{fun: fun, respond: respond} do
    respond.([{:ok, 418, ~s({"error":"teapot","code":"brand_new","next":"upgrade the sdk"})}])
    assert {:error, error} = Pilots.list_hosts(client(fun))
    assert {error.code, error.next} == {"brand_new", "upgrade the sdk"}
  end

  test "a non-zero exit is a result, not an error", %{fun: fun, respond: respond} do
    respond.([{:ok, 200, ~s({"stdout":"","stderr":"boom\\n","exit_code":2})}])
    assert {:ok, result} = Pilots.exec(client(fun), "m-1", "false")
    assert result["exit_code"] == 2
  end

  test "an exec timeout extends the client deadline with a margin",
       %{fun: fun, calls: calls} do
    c = client(fun, timeout: 1_000)
    Pilots.exec(c, "m-1", "sleep 100", timeout_ms: 120_000)

    call = hd(calls.())
    assert call.timeout == 125_000
    assert body_of(call) == %{"cmd" => "sleep 100", "timeout_ms" => 120_000}
  end

  test "a build reads its verdict from the last line", %{fun: fun, respond: respond} do
    lines =
      [
        ~s({"step":"1/2","line":"FROM node","ts":1}),
        ~s({"ts":2,"result":"rootfs-abc","release":"rel-1"})
      ]
      |> Enum.join("\n")

    respond.([{:ok, 200, lines <> "\n"}])
    assert {:ok, parsed} = Pilots.build(client(fun), "tar bytes", deploy: "svc_1")
    assert length(parsed) == 2
    assert {:ok, "rootfs-abc"} = Pilots.build_result(parsed)
  end

  test "a failed build is an error carrying every line", %{fun: fun, respond: respond} do
    lines =
      [
        ~s({"step":"1/1","line":"boom","ts":1}),
        ~s({"ts":2,"error":"npm ci failed","code":"build_failed"})
      ]
      |> Enum.join("\n")

    respond.([{:ok, 200, lines}])
    {:ok, parsed} = Pilots.build(client(fun), "tar")

    assert {:error, %Error{message: "npm ci failed", code: "build_failed"}} =
             Pilots.build_result(parsed)
  end

  test "a build that ended with no verdict is a failure, not a success" do
    assert {:error, %Error{}} = Pilots.build_result([%{"step" => "1/2", "line" => "..."}])
    assert {:error, %Error{}} = Pilots.build_result([])
  end

  test "a build sends the tar as a tar, and passes deploy through",
       %{fun: fun, calls: calls} do
    Pilots.build(client(fun), "tar bytes", deploy: "svc_1")
    call = hd(calls.())
    assert {_url, _headers, ~c"application/x-tar", "tar bytes"} = call.request
    assert String.contains?(url_of(call), "deploy=svc_1")
    # A build outlives any reasonable deadline, so it gets none.
    assert call.timeout == :infinity
  end

  test "a deploy and a rollback get no client deadline either", %{fun: fun, calls: calls} do
    c = client(fun)
    Pilots.deploy(c, "svc_1", build: "rootfs-abc")
    Pilots.rollback(c, "svc_1")
    assert Enum.all?(calls.(), &(&1.timeout == :infinity))
  end

  test "a restore is one request against the checkpoint, creating nothing",
       %{fun: fun, calls: calls} do
    Pilots.restore(client(fun), "ck-1")
    assert [call] = calls.()
    assert call.method == :post
    assert url_of(call) == "http://fleet.test/v1/checkpoints/ck-1/restore"
  end

  test "an id is encoded into the path rather than interpolated raw",
       %{fun: fun, calls: calls} do
    Pilots.get_machine(client(fun), "a/b?c")
    assert url_of(hd(calls.())) == "http://fleet.test/v1/machines/a%2Fb%3Fc"
  end

  test "a body-less POST is still a four-tuple, which is the only form :httpc posts",
       %{fun: fun, calls: calls} do
    c = client(fun)
    Pilots.suspend(c, "m_1")
    Pilots.wake(c, "m_1")
    Pilots.restore(c, "ck-1")
    Pilots.rollback(c, "svc_1")

    # `:httpc.request/4` takes `{url, headers}` only for get/head/delete/
    # options/trace. For a post it answers `{:error, :invalid_request}`
    # without opening a socket, so a two-tuple here would make every one of
    # these calls fail in a way that reads like a network error.
    for call <- calls.() do
      assert call.method == :post
      assert tuple_size(call.request) == 4, "a post built #{inspect(call.request)}"
      assert body_of(call) == %{}
    end

    # A GET keeps the two-tuple form, which is the one `:httpc` wants there.
    Pilots.list_machines(c)
    assert tuple_size(List.last(calls.()).request) == 2
  end

  test "a transport failure is an error rather than a crash", %{fun: fun, respond: respond} do
    respond.([{:error, Error.transport("connection refused")}])
    assert {:error, %Error{status: 0}} = Pilots.list_machines(client(fun))
  end

  test "the bang variants raise the same struct", %{fun: fun, respond: respond} do
    respond.([{:ok, 404, ~s({"error":"gone","code":"not_found","next":"check the id"})}])

    assert_raise Error, ~r/gone \(next: check the id\)/, fn ->
      Pilots.list_machines!(client(fun))
    end
  end
end
