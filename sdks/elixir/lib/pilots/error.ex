defmodule Pilots.Error do
  @moduledoc """
  What a failed call returns.

  Every function in this client answers `{:ok, value}` or `{:error, %Pilots.Error{}}`,
  which is what an Elixir caller expects to pattern-match on. The bang variants
  (`create!`, `exec!`) raise the same struct for a caller who would rather let
  it crash.

  `code`, `next` and `details` come straight from the server's body and are
  present on EVERY error, not only on the ones this version has a name for. A
  code this client has never heard of still reaches the caller with its next
  step attached, which is the difference between an actionable failure and a
  status number.
  """

  defexception [:message, :status, :body, :code, :next, :details]

  @type t :: %__MODULE__{
          message: String.t(),
          status: non_neg_integer(),
          body: String.t(),
          code: String.t(),
          next: String.t(),
          details: term()
        }

  @doc """
  Builds an error from a response.

  The message is the server's own `error` field when there is one, because it
  is written for a person and this client has nothing better to say.
  """
  @spec from_response(non_neg_integer(), String.t(), String.t()) :: t()
  def from_response(status, body, context) do
    decoded =
      case Jason.decode(body) do
        {:ok, %{} = map} -> map
        _ -> %{}
      end

    %__MODULE__{
      message: Map.get(decoded, "error") || "#{context} failed with #{status}",
      status: status,
      body: body,
      code: Map.get(decoded, "code", ""),
      next: Map.get(decoded, "next", ""),
      details: Map.get(decoded, "details")
    }
  end

  @spec transport(String.t()) :: t()
  def transport(message) do
    %__MODULE__{message: message, status: 0, body: "", code: "", next: "", details: nil}
  end

  @impl true
  def message(%__MODULE__{message: message, next: next}) when next != "" do
    "#{message} (next: #{next})"
  end

  def message(%__MODULE__{message: message}), do: message

  @doc "True when the failure is the one this code names, e.g. `not_found`."
  @spec code?(t(), String.t()) :: boolean()
  def code?(%__MODULE__{code: code}, want), do: code == want
end
