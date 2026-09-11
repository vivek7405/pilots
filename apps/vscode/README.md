# pilots for VS Code

A machine is a folder you are editing.

For Neovim, `apps/nvim` does the same thing over a `pilot://` URL scheme. Its
guest commands are a port of this extension's `src/guest.ts`, so a fix to the
shell quoting, the listing or the payload bound in one is a fix owed to the
other.

`pilots: Open Machine` adds a `pilot://<name>/<path>` workspace folder, and
from then on every ordinary thing the editor does happens inside the machine:
open, edit, save, search, rename, drag a file in. `pilots: Open Terminal` puts
a real shell in the same machine beside it, so the editor and the terminal
agree about what is on disk.

## Commands

| Command | What it does |
| --- | --- |
| `pilots: Set API Token` | Stores the token in VS Code's secret storage. Never a setting: a setting is synced and readable by every other extension. |
| `pilots: Open Machine` | Adds the machine as a workspace folder, beside whatever you already had open. |
| `pilots: Create Machine` | Creates one and offers to open it. |
| `pilots: Open Terminal` | `pilot console <name>` in a terminal panel. |
| `pilots: Destroy Machine` | Destroys it, after a confirmation, and removes its folder from the workspace. |
| `pilots: Refresh Files` | Drops the stat cache, for when something changed under you. |
| `Download to Local` | On a `pilot://` file, in the explorer or the editor tab menu. |

## Settings

| Setting | Default | Notes |
| --- | --- | --- |
| `pilots.apiUrl` | empty | The fleet's API URL. Empty uses `PILOT_API_URL`, then `https://api.pilotrun.app`. |
| `pilots.home` | `/home/pilot` | The directory Open Machine starts at. |

## How it works

Every read and write is a command in the guest over the SDK's buffered exec,
base64-encoded so binaries, CRLF and unicode survive a shell. There is no agent
to install in the machine and no port to open: the same exec route an agent
uses carries the bytes.

Two decisions are worth knowing:

- **A listing costs one round trip**, not one per entry. A `stat` per file is
  what makes a remote filesystem feel broken on a directory of any size.
- **Only `stat` is cached, and only for two seconds.** A filesystem the editor
  caches is a filesystem that disagrees with the terminal running beside it.

`src/guest.ts` holds the commands and the parsing and imports no editor API,
which is why `npm test` can cover the shell quoting, the `stat` parsing and the
error classification without an extension host.

## Building

```sh
npm install            # from the repository root
npm test --workspace=apps/vscode
npm run package --workspace=apps/vscode   # writes pilots-vscode-<version>.vsix
```

Publishing to the marketplace is a manual step: `npx @vscode/vsce publish` with
a publisher token. Nothing in CI does it, because a release should be a
deliberate act.
