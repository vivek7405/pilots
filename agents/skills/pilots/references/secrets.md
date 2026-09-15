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

## Machine credentials

A machine holds no API key, and that is deliberate. A key baked into a guest is a key in every snapshot of it, in every fork of it, and in whatever that guest writes to object storage, and re-issuing one on restore or rescue would be a fleet-wide path that has to stay correct forever.

So a machine ASKS. hostd binds a broker inside each machine's own network namespace, on the gateway address the guest already routes to, and the request path is the identity: nothing is presented, because there is nothing a guest could present that a copy of that guest could not.

**Deny by default.** With no grant, a machine can mint no token and read no secret.

| I need to... | Do |
| --- | --- |
| let a machine call the API | `grant` with `scopes: ["machines"]` |
| let a service's replicas call it | `grant` with a `service` and the scopes; every later deploy inherits it |
| give a machine a secret | `grant` with `secrets: {NAME: value}` |
| see what is granted | `grants` (names and scopes, never values) |
| take it away | `grant` with an empty request |

### Inside the machine

| Variable | What it is |
| --- | --- |
| `PILOT_BROKER_URL` | where to ask. `http://169.254.0.22:3002` on every machine |
| `PILOT_TOKEN_FILE` | where the guest agent keeps a fresh token, `/run/pilot/token` |
| `PILOT_MACHINE_ID` | this machine's own id |
| `PILOT_API_URL` | the fleet's API |

There is deliberately no `PILOT_TOKEN`. A token in the environment is a token in `/etc/pilot/env`, which is on disk in every snapshot and stale fifteen minutes later. The agent keeps the FILE fresh instead, and the SDKs read it, so an SDK client constructed with no key inside a machine just works.

```sh
# From inside a machine, by hand:
curl -s "$PILOT_BROKER_URL/identity"
curl -s "$PILOT_BROKER_URL/token"
curl -s "$PILOT_BROKER_URL/secrets"
```

### What a machine's token may do

Reads are org-wide: a machine can list its siblings, which lets it find its own service and resolve a peer. WRITES are narrowed to that machine and to its own service, and a shell or a raw tunnel counts as a write however it is spelled. A compromised replica restarting its peers one by one is an outage it can cause alone, so it cannot.

`admin` is never brokerable. A token lives fifteen minutes and is stopped by its own expiry, by revoking its hash through the API-key revoke route, or by destroying the machine.

## Do not

- Do not put a long-lived API key in a machine's environment to let it call the API. Grant it instead: the key would be in every snapshot and fork, and would outlive the machine.
- Do not expect a granted secret to appear in the environment. It deliberately does not; the machine fetches it from the broker, which is what keeps it out of every snapshot.
