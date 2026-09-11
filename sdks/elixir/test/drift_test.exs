defmodule Pilots.DriftTest do
  @moduledoc """
  The drift test, in the shape this client needs one.

  The other three SDKs mirror hostd's structs as types and compare field for
  field. This one returns plain maps with the server's own keys, so there is
  no second copy of a field name to rot. What CAN rot is the ROUTE TABLE: a
  function here names a path as a string, and a path hostd renamed or removed
  would fail only at runtime, against a real fleet, for whoever ran it first.

  So the check is that every path this module spells out is a path hostd's own
  route table registers. It parses `internal/api/routes.go`, which is the file
  that decides what exists.
  """
  use ExUnit.Case, async: true

  @api_dir Path.expand("../../../apps/hostd/internal/api", __DIR__)

  defp hostd_routes do
    routes_go = Path.join(@api_dir, "routes.go")

    routes_go
    |> File.read!()
    |> then(&Regex.scan(~r/mux\.Handle(?:Func)?\("(?:([A-Z]+) )?([^"]+)"/, &1))
    |> Enum.map(fn
      [_, _method, path] -> path
    end)
    |> Enum.map(&normalise/1)
    |> MapSet.new()
  end

  # `{id}` and `{name}` are one shape as far as this test is concerned: what
  # it checks is that the SEGMENTS around them are the ones hostd serves.
  defp normalise(path), do: Regex.replace(~r/\{[a-zA-Z]+\}/, path, "{}")

  defp client_paths do
    lib = Path.expand("../lib", __DIR__)

    for file <- Path.wildcard(Path.join(lib, "**/*.ex")),
        line <- File.read!(file) |> String.split("\n"),
        # A path in this client is always a string starting `/v1/` (or `/mcp`),
        # interpolated with `#{...}` where hostd has a `{param}`.
        capture <- Regex.scan(~r|"(/v1/[^"]*)"|, line),
        [_, path] = capture,
        into: MapSet.new() do
      path
      |> String.replace(~r/\#\{[^}]*\}/, "{}")
      |> normalise()
    end
  end

  test "hostd's route table was found" do
    routes = hostd_routes()

    # A moved file must fail loudly rather than pass vacuously by finding
    # nothing on either side.
    assert MapSet.size(routes) >= 20,
           "only #{MapSet.size(routes)} routes parsed from #{@api_dir}; the file moved?"

    assert MapSet.member?(routes, "/v1/machines")
  end

  test "every path this client calls is one hostd serves" do
    routes = hostd_routes()
    mine = client_paths()

    assert MapSet.size(mine) >= 15,
           "only #{MapSet.size(mine)} paths found in lib/; the parse broke?"

    unknown = MapSet.difference(mine, routes) |> MapSet.to_list() |> Enum.sort()

    assert unknown == [],
           """
           These paths are not in hostd's route table:

             #{Enum.join(unknown, "\n  ")}

           Either the route was renamed and this client was not, or the path
           was typed wrong. hostd's table is apps/hostd/internal/api/routes.go.
           """
  end

  test "the routes a client of this shape needs are all reachable" do
    mine = client_paths()

    # Not every hostd route belongs in every client, but these are the ones a
    # caller of THIS module can reach, and a silent removal of one would be a
    # feature quietly disappearing from the Elixir surface.
    for path <- [
          "/v1/machines",
          "/v1/machines/{}",
          "/v1/machines/{}/exec",
          "/v1/machines/{}/checkpoints",
          "/v1/machines/{}/promote",
          "/v1/checkpoints/{}/restore",
          "/v1/services",
          "/v1/services/{}/deploy",
          "/v1/builds",
          "/v1/plan",
          "/v1/whoami",
          "/v1/health"
        ] do
      assert MapSet.member?(mine, path), "the client no longer calls #{path}"
    end
  end
end
