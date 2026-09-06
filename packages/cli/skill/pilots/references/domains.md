# Domains

## What this covers

Serving an app on a hostname the user owns. Not covered: the URL every service already has, which is minted automatically and never changes (deploy.md).

## The command to copy

    pilot domains add --service web --hostname app.example.com

MCP: `domains` lists what exists and whether each is verified. ADDING one is the user's command, because it needs a DNS record only they can create.

## What happens

1. `pilot domains add` records the hostname and prints the DNS record to create.
2. The user creates the record at their registrar.
3. Verification passes, and a certificate is issued for the name.
4. The service serves on both its own URL and the custom domain. The original URL keeps working.

## The rules

| Rule | Why |
| --- | --- |
| the user adds the domain | it needs a DNS record at their registrar |
| a name under the fleet's own domain is refused | that name is already minted for them; accepting it would let one tenant claim another's URL |
| verification comes before a certificate | an unverified name would spend the fleet's shared certificate rate limit on a name nobody owns |

## Each answer and what to do

| Answer | Meaning | Do |
| --- | --- | --- |
| `verified: false` | the DNS record is not visible yet | tell the user which record to create; DNS takes minutes |
| `bad_request` about the fleet domain | the hostname is under the fleet's own apex | use a name the user owns |

## Do not

- Do not add a domain for the user. Print the command and the record.
- Do not wait in a loop for verification. DNS propagation is not something to poll through.
