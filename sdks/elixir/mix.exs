defmodule Pilots.MixProject do
  use Mix.Project

  @version "0.1.0"
  @source_url "https://github.com/vivek7405/pilots"

  def project do
    [
      app: :pilots,
      version: @version,
      elixir: "~> 1.15",
      start_permanent: Mix.env() == :prod,
      deps: deps(),
      description:
        "Client for pilots: instant sandboxes and durable production services on Firecracker microVMs, one primitive.",
      package: package(),
      docs: docs(),
      name: "pilots",
      source_url: @source_url
    ]
  end

  # `:inets` and `:ssl` are OTP's own, which is the whole dependency story:
  # the HTTP calls go through `:httpc` and the exec stream through a websocket
  # this package implements, so a consumer adds one line to mix.exs and pulls
  # in nothing that is not already on their machine.
  def application do
    [extra_applications: [:logger, :inets, :ssl, :crypto]]
  end

  defp deps do
    [
      # JSON is the one thing OTP 27 does not have a stable public API for
      # (`:json` landed in OTP 27 but Elixir 1.18 still targets 26 too).
      {:jason, "~> 1.4"},
      {:ex_doc, "~> 0.34", only: :dev, runtime: false}
    ]
  end

  defp package do
    [
      licenses: ["Apache-2.0"],
      links: %{"GitHub" => @source_url, "Docs" => "https://pilots.run/agents"},
      files: ~w(lib mix.exs README.md)
    ]
  end

  defp docs do
    [main: "readme", extras: ["README.md"]]
  end
end
