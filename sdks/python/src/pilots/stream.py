"""The streaming exec.

The wire format is sprites' byte protocol, so an existing sprites client
drops in unchanged: every binary frame's first byte is the stream id --
1 stdout, 2 stderr, 3 exit with the code in ``payload[0]`` -- and the two the
client sends are 0 for a stdin chunk and 4 for stdin EOF.

Two behaviours here are load-bearing rather than stylistic:

* ``stdin`` defaults to False. A process holding an open stdin it never reads
  hangs, and an agent run under ``claude -p`` is exactly such a process.
* a close with no exit frame is an ERROR, never a silent code 0. The guest
  agent drains both output pumps before it writes the exit frame and
  websocket frames are ordered, so an exit frame means the output that
  preceded it has already arrived. A socket that dropped instead means nobody
  knows what the command did.

``tty`` is a mode on this same stream, not a second protocol: a PTY merges the
two output streams onto ``stdout``, stdin is always read, ``end_stdin()``
sends EOT rather than closing anything, and ``resize()`` becomes callable.
"""

from __future__ import annotations

import json
import queue
import threading
from collections.abc import Iterator
from typing import Any
from urllib.parse import urlencode

from .errors import PilotsError
from .types import FrameExit, FrameStderr, FrameStdin, FrameStdinEOF, FrameStdout

_EOF = object()


def build_exec_url(
    base_url: str,
    path: str,
    argv: list[str],
    *,
    cwd: str | None = None,
    env: dict[str, str] | None = None,
    user: str | None = None,
    stdin: bool = False,
    tty: bool = False,
    rows: int | None = None,
    cols: int | None = None,
    org: str | None = None,
) -> str:
    """Builds the exec-stream URL, with the query names sprites uses."""
    if base_url.startswith("http"):
        base_url = "ws" + base_url[len("http") :]
    params: list[tuple[str, str]] = []
    # The org narrowing reaches THIS route too; every other call applies it
    # in Http, and this one builds its own URL.
    if org:
        params.append(("org", org))
    for arg in argv:
        params.append(("cmd", arg))
    if argv:
        params.append(("path", argv[0]))
    if cwd:
        params.append(("dir", cwd))
    for k, v in (env or {}).items():
        params.append(("env", f"{k}={v}"))
    if user:
        params.append(("user", user))
    # Always present, never inferred: the default is the thing most likely to
    # be wrong by omission. A tty forces it on; hostd refuses the pair
    # tty=true&stdin=false with a 400.
    params.append(("stdin", "true" if (tty or stdin) else "false"))
    if tty:
        params.append(("tty", "true"))
        if rows is not None:
            params.append(("rows", str(rows)))
        if cols is not None:
            params.append(("cols", str(cols)))
    return f"{base_url}{path}?{urlencode(params)}"


class _Pipe:
    """A readable end fed by the reader thread: iterate it for chunks, or
    ``read()`` it whole once the stream has exited."""

    def __init__(self) -> None:
        self._q: queue.Queue[Any] = queue.Queue()
        self._closed = False

    def _push(self, chunk: bytes) -> None:
        if not self._closed:
            self._q.put(chunk)

    def _close(self) -> None:
        if not self._closed:
            self._closed = True
            self._q.put(_EOF)

    def __iter__(self) -> Iterator[bytes]:
        while True:
            item = self._q.get()
            if item is _EOF:
                self._q.put(_EOF)
                return
            yield item

    def read(self) -> bytes:
        """Everything until EOF, in one bytes object."""
        return b"".join(self)


class ExecStream:
    """A running command.

    ``stdout`` and ``stderr`` are queues fed by a reader thread. A WebSocket
    cannot be paused, so those queues are the only boundary: a stream nobody
    reads grows without limit. For output nobody intends to read, use the
    buffered ``machines.exec``.
    """

    def __init__(self, url: str, api_key: str, *, stdin: bool, tty: bool, open_timeout: float = 30.0) -> None:
        from websockets.sync.client import connect

        self.stdout = _Pipe()
        self.stderr = _Pipe()
        self._stdin_enabled = stdin or tty
        self._tty = tty
        self._code: int | None = None
        self._exited = False
        self._error: Exception | None = None
        self._done = threading.Event()
        self._lock = threading.Lock()
        try:
            # The key travels as a subprotocol rather than a header: one code
            # path shared with browsers, and hostd accepts either form.
            self._ws = connect(
                url,
                subprotocols=[f"authorization.bearer.{api_key}"],  # type: ignore[list-item]
                open_timeout=open_timeout,
                max_size=None,
            )
        except Exception as cause:
            raise PilotsError(f"exec stream could not connect: {cause}") from cause
        self._reader = threading.Thread(target=self._pump, name="pilots-exec-stream", daemon=True)
        self._reader.start()

    # -- the reader -----------------------------------------------------------

    def _pump(self) -> None:
        try:
            for message in self._ws:
                if isinstance(message, str):
                    self._on_text(message)
                elif message:
                    self._on_frame(bytes(message))
        except Exception as err:  # the socket dropped
            self._fail(PilotsError(f"exec stream failed: {err}"))
        finally:
            self._end_pipes()
            if not self._exited:
                self._fail(PilotsError("stream closed before exit"))
            self._done.set()

    def _on_frame(self, frame: bytes) -> None:
        kind, payload = frame[0], frame[1:]
        if kind == FrameStdout:
            if not self._exited:
                self.stdout._push(payload)
        elif kind == FrameStderr:
            if not self._exited:
                self.stderr._push(payload)
        elif kind == FrameExit:
            self._finish(payload[0] if payload else 0)
        # An id this version does not know is not a reason to fail.

    def _on_text(self, data: str) -> None:
        # hostd sends a text {"type":"exit","exit_code":n} BEFORE the binary
        # exit frame, because the binary one carries the code in one byte and
        # a signal death (-1) cannot be told from 255 there. Whichever arrives
        # first decides; the other is ignored.
        try:
            parsed = json.loads(data)
        except ValueError:
            return
        if isinstance(parsed, dict) and parsed.get("type") == "exit" and not self._exited:
            self._finish(int(parsed.get("exit_code", 0)))

    def _finish(self, code: int) -> None:
        with self._lock:
            if self._exited:
                return
            self._exited = True
            self._code = code
        self._end_pipes()
        try:
            self._ws.close()
        except Exception:
            pass

    def _end_pipes(self) -> None:
        self.stdout._close()
        self.stderr._close()

    def _fail(self, err: Exception) -> None:
        with self._lock:
            if self._error is None and not self._exited:
                self._error = err

    # -- the caller's side ------------------------------------------------------

    @property
    def exit_code(self) -> int | None:
        """The exit code, or None until the exit frame arrives."""
        return self._code

    def wait(self, timeout: float | None = None) -> int:
        """Blocks until exit and returns the code; raises on a failure or a
        close before exit."""
        if not self._done.wait(timeout):
            raise PilotsError("timed out waiting for the command to exit")
        if self._error is not None:
            raise self._error
        assert self._code is not None
        return self._code

    def output(self, timeout: float | None = None) -> tuple[bytes, bytes, int]:
        """Waits for the exit, then drains both pipes: ``(stdout, stderr, exit_code)``.

        The WAIT comes first, and the order is the whole reason ``timeout``
        means anything: a pipe is only closed by the exit frame or a dropped
        socket, so draining first blocks forever on a command that is alive and
        silent -- and ``timeout`` would bound nothing. Once the exit has
        arrived, both pipes are already at EOF and the reads return at once.
        """
        code = self.wait(timeout)
        out_box: list[bytes] = []
        err_box: list[bytes] = []
        t = threading.Thread(target=lambda: err_box.append(self.stderr.read()), daemon=True)
        t.start()
        out_box.append(self.stdout.read())
        t.join()
        return out_box[0], err_box[0], code

    def write_stdin(self, chunk: bytes | str) -> None:
        """Sends one stdin chunk (frame 0). Raises unless the stream opted into stdin."""
        if not self._stdin_enabled:
            raise PilotsError("this stream was opened with stdin=False")
        data = chunk.encode() if isinstance(chunk, str) else bytes(chunk)
        self._ws.send(bytes([FrameStdin]) + data)

    def end_stdin(self) -> None:
        """Closes the process's stdin (frame 4). Under ``tty`` this sends EOT
        to the terminal instead, and the session stays open."""
        if not self._stdin_enabled:
            raise PilotsError("this stream was opened with stdin=False")
        self._ws.send(bytes([FrameStdinEOF]))

    def resize(self, cols: int, rows: int) -> None:
        """Resizes the terminal. Raises unless the stream was opened with ``tty``."""
        if not self._tty:
            raise PilotsError("this stream was opened without tty")
        self._ws.send(json.dumps({"type": "resize", "cols": cols, "rows": rows}))

    def kill(self) -> None:
        """Closes the socket. The agent's context cancel kills the process."""
        try:
            self._ws.close()
        except Exception:
            pass

    close = kill

    def __enter__(self) -> ExecStream:
        return self

    def __exit__(self, *exc: object) -> None:
        self.kill()
