# Secrets

## What this covers

A password, token or key an app needs in its environment, without the value passing through a model or a shell history. Not covered: non-secret configuration, which is plain `env` in a compose file (compose.md).

## The command to copy

    pilot secrets set DATABASE_URL      # prompts; the value is never an argument
    pilot secrets import .env
    pilot secrets ls

In a compose file the reference, not the value:

    services:
      web:
        environment:
          DATABASE_URL: secret://database_url

## What happens

1. `pilot secrets set` prompts for the value and stores it locally, keyed by the app the compose file names.
2. `pilot deploy` resolves each `secret://` reference and sends the values as `secret_env`.
3. The host seals them with the fleet key before they touch any replicated row, so a secret is never readable from the state store.
4. `service` returns env KEYS and never values, on purpose.

## The rules

| Rule | Why |
| --- | --- |
| the user runs `pilot secrets set`, not you | the value must not pass through the model or the conversation |
| a compose file carries `secret://name`, never a value | the file is committed; the value is not |
| a host with no fleet key refuses `secret_env` | it would otherwise write a plaintext secret into every replica of the state store |

## Each answer and what to do

| Answer | Meaning | Do |
| --- | --- | --- |
| `not_configured` naming `PILOT_FLEET_KEY` | the fleet cannot seal secrets | tell the user; send plain `env` only if the value is not secret |
| a `secret://` reference with nothing stored | the deploy has no value for it | ask the user to run `pilot secrets set <name>` |

## Do not

- Do not put a value in a compose file, a tool argument, or a message.
- Do not read a secret back to confirm it. Nothing returns one.
- Do not pass a secret as `env`. It would be stored unsealed.
