# pilots for Neovim

A machine is a folder you are editing.

`:PilotsOpen scratch` opens a listing of the machine, `<CR>` descends into a
directory or opens a file, and `:w` writes it back into the guest.
`:PilotsTerminal scratch` puts a real shell in the same machine beside it, so
the editor and the terminal agree about what is on disk.

## Install

```lua
-- lazy.nvim
{ "vivek7405/pilots.nvim", cmd = { "PilotsOpen", "PilotsTerminal" } }
```

It needs the `pilot` CLI on PATH and a machine you can reach. Nothing is
installed in the machine and no port is opened.

## Commands

| Command | What it does |
| --- | --- |
| `:PilotsOpen <machine> [path]` | Opens the machine, at `path` or at home. |
| `:PilotsTerminal <machine>` | `pilot console <machine>` in a split. |

You can also just `:e pilot://scratch/etc/hosts`. The commands are a
convenience over the URL, not a different mechanism.

## URLs

| URL | Means |
| --- | --- |
| `pilot://scratch` | the home directory, `/home/pilot` |
| `pilot://scratch/` | the guest root |
| `pilot://scratch/etc/hosts` | that file |

The difference between the first two is load-bearing. If both meant home,
climbing out of `/home` would land back in home and the root could never be
reached.

## Settings

```lua
require("pilots").setup({
  cmd = "pilot",          -- the binary, when it is not on PATH
  home = "/home/pilot",   -- where a bare machine name opens
  timeout_ms = 60000,     -- how long one guest command may take
})
```

`setup()` is optional. The plugin works the moment it is installed, because a
user who types `:e pilot://scratch/` before configuring anything should get
their machine rather than an empty buffer named after a URL.

## How it works

Every read and write is a command in the guest, run through `pilot exec` and
base64-encoded so binaries, CRLF and unicode survive a shell. The CLI already
solves authentication, machine-name resolution and the exec route, so none of
that is reimplemented here.

Three decisions are worth knowing:

- **A listing costs one round trip**, not one per entry. A `stat` per file is
  what makes a remote filesystem feel broken on a directory of any size.
- **A write is chunked at 48000 base64 characters.** Linux caps a single argv
  string at `MAX_ARG_STRLEN`, and the whole command is one argument to
  `sh -c`, so a larger payload fails the exec itself and the guest agent
  reports it as exit 127 with empty stderr, naming nothing.
- **A failed write leaves the buffer modified.** A buffer marked clean after a
  write that did not land is how an edit is lost: the next `:q` asks nothing.

## What it does not do

No LSP on `pilot://` buffers. Language servers want real paths on a real disk,
and pretending otherwise would produce diagnostics about a file that is not
there. `apps/vscode` has the same gap.

No project-wide search over a machine yet.

## Tests

```sh
nvim -l apps/nvim/test/run.lua
```

Plain Lua rather than busted, which is a luarocks install: a suite that needs
one more system dependency than the plugin itself is a suite that eventually
gets skipped. Neovim is already required to use this.
