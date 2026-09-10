"""``pilots.sprites_compat`` -- the sprites-shaped face of the pilots API.

It exists so a codebase written against ``sprites-py`` moves providers by
changing one import line. The surface is what a sprites client consumes and
nothing more: no services, no ``/control`` multiplex, no network policy.

Four rules decide the shapes here.

* **A sprite's ``id`` is the machine's NAME.** A sprites consumer persists the
  id and hands it straight back as a path segment. ``machine_id`` carries the
  ``m-…`` id for calls made through the typed client. Either form works as an
  argument: a value that matches no name but looks like a machine id is looked
  up as one.
* **``restore_checkpoint`` restores IN PLACE.** Exactly one request, and no
  machine is created. A machine created in a restore would get a new URL, and
  a URL is permanent.
* **``run`` returns a ``subprocess.CompletedProcess``**, which is what
  ``sprites-py`` returns and what a caller destructures.
* **``set_public_url`` is a no-op.** A workload's URL is public here by
  default, so there is nothing to switch on.
"""

from __future__ import annotations

import os
import subprocess
from dataclasses import dataclass
from datetime import datetime, timezone
from typing import Any

from ._http import DEFAULT_TIMEOUT
from .client import PilotsClient
from .errors import NotFoundError, PilotsError
from .stream import ExecStream
from .types import Machine

__all__ = [
    "SpritesClient",
    "Sprite",
    "SpriteCommand",
    "SpriteFilesystem",
    "Checkpoint",
    "ExecResult",
    "URLSettings",
]


@dataclass
class URLSettings:
    """Accepted for source compatibility. ``auth`` is ``"public"`` or
    ``"sprite"``; pilots maps the latter onto ``url_auth="org"``."""

    auth: str = "public"
    private_access: str | None = None


@dataclass
class ExecResult:
    """What a sprites consumer destructures from an exec."""

    stdout: str
    stderr: str
    exit_code: int


@dataclass
class Checkpoint:
    id: str
    create_time: datetime | None = None
    comment: str = ""


def _looks_like_machine_id(ref: str) -> bool:
    return ref.startswith("m-") or ref.startswith("m_")


class SpriteFilesystem:
    """The sprites filesystem object, over exec. Reads and writes run as
    commands inside the machine (base64, so quotes, newlines and unicode
    survive), which keeps them consistent with everything else the agent does
    and correctly reverted by a checkpoint restore."""

    def __init__(self, sprite: Sprite, working_dir: str = "/") -> None:
        self._sprite = sprite
        self.working_dir = working_dir or "/"

    def _abs(self, path: str) -> str:
        if path.startswith("/"):
            return path
        base = self.working_dir.rstrip("/")
        return f"{base}/{path}" if base else f"/{path}"

    def read_text(self, path: str) -> str:
        import base64

        res = self._sprite._exec(f"base64 -w0 -- {_quote(self._abs(path))}")
        if res.exit_code != 0:
            raise PilotsError(res.stderr.strip() or f"cannot read {path}", status=0, next="check the path")
        return base64.b64decode(res.stdout).decode()

    def write_text(self, path: str, text: str) -> None:
        import base64

        target = self._abs(path)
        encoded = base64.b64encode(text.encode()).decode()
        res = self._sprite._exec(
            f"mkdir -p -- $(dirname {_quote(target)}) && printf %s {_quote(encoded)} | base64 -d > {_quote(target)}"
        )
        if res.exit_code != 0:
            raise PilotsError(res.stderr.strip() or f"cannot write {path}")

    def listdir(self, path: str = ".") -> list[str]:
        res = self._sprite._exec(f"ls -1A -- {_quote(self._abs(path))}")
        if res.exit_code != 0:
            raise PilotsError(res.stderr.strip() or f"cannot list {path}")
        return [line for line in res.stdout.splitlines() if line]

    def exists(self, path: str) -> bool:
        return self._sprite._exec(f"test -e {_quote(self._abs(path))}").exit_code == 0

    def remove(self, path: str) -> None:
        self._sprite._exec(f"rm -rf -- {_quote(self._abs(path))}")


def _quote(s: str) -> str:
    return "'" + s.replace("'", "'\\''") + "'"


class SpriteCommand:
    """The Go-style ``exec.Cmd`` face: build it, then take its output."""

    def __init__(self, sprite: Sprite, argv: list[str], *, cwd: str | None, env: dict[str, str] | None, tty: bool,
                 tty_rows: int | None, tty_cols: int | None, timeout: float | None) -> None:
        self._sprite = sprite
        self.argv = argv
        self.cwd = cwd
        self.env = env
        self.tty = tty
        self.tty_rows = tty_rows
        self.tty_cols = tty_cols
        self.timeout = timeout
        self.exit_code: int | None = None

    def _stream(self, stdin: bool = False) -> ExecStream:
        return self._sprite._client.machines.exec_stream(
            self._sprite.machine_id,
            self.argv,
            cwd=self.cwd,
            env=self.env,
            stdin=stdin or self.tty,
            tty=self.tty,
            rows=self.tty_rows,
            cols=self.tty_cols,
        )

    def output(self) -> bytes:
        """stdout. Raises when the command exited non-zero, as sprites does."""
        out, err, code = self._stream().output(self.timeout)
        self.exit_code = code
        if code != 0:
            raise PilotsError(err.decode(errors="replace").strip() or f"exit status {code}")
        return out

    def combined_output(self) -> bytes:
        out, err, code = self._stream().output(self.timeout)
        self.exit_code = code
        return out + err

    def run(self) -> int:
        _, _, code = self._stream().output(self.timeout)
        self.exit_code = code
        return code

    def start(self, stdin: bool = False) -> ExecStream:
        """The raw stream, for a caller that wants the pipes."""
        return self._stream(stdin=stdin)


class Sprite:
    """One machine, in sprites clothing."""

    def __init__(self, client: PilotsClient, machine: Machine | None, name: str) -> None:
        self._client = client
        self._machine = machine
        self._name = name

    # -- identity -------------------------------------------------------------

    @property
    def id(self) -> str:
        """The NAME. A sprites consumer persists this and hands it back."""
        return self._machine.name if self._machine else self._name

    @property
    def name(self) -> str:
        return self.id

    @property
    def machine_id(self) -> str:
        """The `m-…` id, for calls through the typed client."""
        return self._resolved().id

    @property
    def url(self) -> str:
        return self._resolved().url

    @property
    def status(self) -> str:
        return self._resolved().state

    @property
    def machine(self) -> Machine:
        return self._resolved()

    def _resolved(self) -> Machine:
        if self._machine is None:
            self._machine = _lookup(self._client, self._name)
        return self._machine

    def refresh(self) -> Sprite:
        self._machine = _lookup(self._client, self.id)
        return self

    # -- exec ------------------------------------------------------------------

    def _exec(self, cmd: str, *, cwd: str | None = None, env: dict[str, str] | None = None,
              timeout: float | None = None) -> ExecResult:
        res = self._client.machines.exec(
            self.machine_id, cmd=cmd, cwd=cwd, env=env, timeout_ms=int(timeout * 1000) if timeout else None
        )
        return ExecResult(res.stdout, res.stderr, res.exit_code)

    def run(
        self,
        *argv: str,
        capture_output: bool = False,
        timeout: float | None = None,
        cwd: str | None = None,
        env: dict[str, str] | None = None,
        check: bool = False,
    ) -> subprocess.CompletedProcess[bytes]:
        """``subprocess.run`` for a machine. Returns a ``CompletedProcess``
        whose ``stdout`` and ``stderr`` are bytes, as sprites-py does."""
        stream = self._client.machines.exec_stream(self.machine_id, list(argv), cwd=cwd, env=env)
        out, err, code = stream.output(timeout)
        if check and code != 0:
            raise subprocess.CalledProcessError(code, list(argv), out, err)
        return subprocess.CompletedProcess(
            list(argv), code, out if capture_output else None, err if capture_output else None
        )

    def command(
        self,
        *argv: str,
        cwd: str | None = None,
        env: dict[str, str] | None = None,
        tty: bool = False,
        tty_rows: int | None = None,
        tty_cols: int | None = None,
        timeout: float | None = None,
    ) -> SpriteCommand:
        return SpriteCommand(
            self, list(argv), cwd=cwd, env=env, tty=tty, tty_rows=tty_rows, tty_cols=tty_cols, timeout=timeout
        )

    def exec(self, cmd: str, **kw: Any) -> ExecResult:
        """The buffered form, taking a command line rather than an argv."""
        return self._exec(cmd, **kw)

    # -- filesystem ------------------------------------------------------------

    def filesystem(self, working_dir: str = "/") -> SpriteFilesystem:
        return SpriteFilesystem(self, working_dir)

    # -- checkpoints -----------------------------------------------------------

    def create_checkpoint(self, comment: str = "") -> Checkpoint:
        ck = self._client.machines.checkpoint(self.machine_id, comment or None)
        return Checkpoint(ck.id, _when(ck.created_at), ck.comment or "")

    def list_checkpoints(self) -> list[Checkpoint]:
        return [
            Checkpoint(c.id, _when(c.created_at), c.comment or "")
            for c in self._client.machines.list_checkpoints(self.machine_id)
        ]

    def restore_checkpoint(self, checkpoint_id: str) -> Sprite:
        """IN PLACE: the same machine, same id, same URL, same token."""
        self._machine = self._client.checkpoints.restore(checkpoint_id)
        return self

    # -- lifecycle -------------------------------------------------------------

    def suspend(self) -> None:
        self._client.machines.suspend(self.machine_id)

    def wake(self) -> None:
        self._client.machines.wake(self.machine_id)

    def destroy(self) -> None:
        self._client.machines.destroy(self.machine_id)

    delete = destroy

    def set_public_url(self, *_: Any, **__: Any) -> None:
        """A no-op: a machine's URL is public here by default."""
        return None

    def update_url_settings(self, settings: URLSettings) -> None:
        self._machine = self._client.machines.update(
            self.machine_id, {"url_auth": "org" if settings.auth == "sprite" else "public"}
        )


class SpritesClient:
    """The sprites-shaped client. ``token`` falls back to ``PILOT_API_KEY``
    (and ``SPRITES_TOKEN``, so an unedited environment still works)."""

    def __init__(
        self,
        token: str | None = None,
        *,
        base_url: str | None = None,
        timeout: float = DEFAULT_TIMEOUT,
        org: str | None = None,
        **_ignored: Any,
    ) -> None:
        key = token or os.environ.get("PILOT_API_KEY") or os.environ.get("SPRITES_TOKEN", "")
        self.client = PilotsClient(key, base_url=base_url, timeout=timeout, org=org)

    def close(self) -> None:
        self.client.close()

    def __enter__(self) -> SpritesClient:
        return self

    def __exit__(self, *exc: object) -> None:
        self.close()

    def sprite(self, name: str) -> Sprite:
        """A lazy handle: nothing is fetched until something needs the id."""
        return Sprite(self.client, None, name)

    def get_sprite(self, name: str) -> Sprite:
        return Sprite(self.client, _lookup(self.client, name), name)

    def create_sprite(
        self,
        name: str,
        *,
        url_settings: URLSettings | None = None,
        labels: list[str] | dict[str, str] | None = None,
        env: dict[str, str] | None = None,
        **_ignored: Any,
    ) -> Sprite:
        body: dict[str, Any] = {"name": name}
        if url_settings is not None and url_settings.auth == "sprite":
            body["url_auth"] = "org"
        if labels:
            # sprites takes a list of strings; pilots takes a map. A bare
            # string becomes a key with an empty value, which is what a tag is.
            body["labels"] = labels if isinstance(labels, dict) else {label: "" for label in labels}
        if env:
            body["env"] = env
        machine = self.client.machines.create(body)
        return Sprite(self.client, machine, machine.name)

    def list_sprites(self, prefix: str | None = None, **_ignored: Any) -> list[Sprite]:
        out = []
        for m in self.client.machines.list():
            if prefix and not m.name.startswith(prefix):
                continue
            out.append(Sprite(self.client, m, m.name))
        return out

    def destroy_sprite(self, name: str) -> None:
        self.sprite(name).destroy()

    delete_sprite = destroy_sprite


def _lookup(client: PilotsClient, ref: str) -> Machine:
    """A name, or a machine id. The alias serving sprites paths resolves
    names, so a name is tried first and an id is the fallback."""
    for m in client.machines.list():
        if m.name == ref:
            return m
    if _looks_like_machine_id(ref):
        return client.machines.get(ref)
    raise NotFoundError(f"no sprite {ref}", next="list_sprites shows what this key can see")


def _when(unix: int) -> datetime | None:
    return datetime.fromtimestamp(unix, tz=timezone.utc) if unix else None
