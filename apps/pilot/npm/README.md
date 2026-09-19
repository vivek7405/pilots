# pilots

The `pilot` CLI and terminal dashboard for [Pilots](https://pilots.run):
instant sandboxes for AI agents and durable production services on Firecracker
microVMs. One primitive, one CLI.

```sh
npm install -g pilots
pilot login
cd my-app && pilot deploy
```

Or run it without installing anything:

```sh
npx pilots deploy
```

The package installs two names for the same command, `pilot` and `pilots`.

## What is in the package

`pilot` is a static Go binary. This package carries the build for every
supported system (Linux and macOS, x64 and arm64) and a small launcher that
runs the one for yours. No install script runs and nothing is downloaded at
install time, so it works with `--ignore-scripts` and behind a registry mirror.

There is no Windows build. Under WSL, which is Linux, this package works
unchanged; on Windows itself the command says so and points there.

Every version is published from GitHub Actions with npm provenance, so the
package page links the exact commit and workflow run that built it.

## Upgrading and removing

```sh
npm install -g pilots@latest
npm rm -g pilots
```

`pilot upgrade` is for a binary installed by the
[install script](https://pilots.run/install). Run from an npm install it
prints the npm command above and changes nothing, because that file is npm's.

Your key lives in `~/.config/pilots/credentials`. Delete it as well and
nothing of the CLI is left on the machine.

## Links

- Install page: https://pilots.run/install
- Release notes: https://pilots.run/changelog
- Source: https://github.com/pilotsrun/pilots (`apps/pilot`), Apache-2.0
