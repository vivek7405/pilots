"""The exec stream against an in-process websocket that speaks hostd's
frames: the URL shape, the subprotocol key, stdin opt-in, the leading text
verdict, and a close with no exit frame."""

from __future__ import annotations

import json
import threading
from collections.abc import Iterator
from typing import Any
from urllib.parse import parse_qs, urlsplit

import pytest
from websockets.sync.server import serve

from pilots import ExecStream, PilotsClient, PilotsError, build_exec_url

SCRIPTS: dict[str, Any] = {}
SEEN: list[dict[str, Any]] = []


def handler(ws: Any) -> None:
    req = ws.request
    query = parse_qs(urlsplit(req.path).query)
    SEEN.append({"path": req.path, "query": query, "subprotocol": ws.subprotocol})
    script = SCRIPTS.get(query.get("cmd", [""])[0])
    if script:
        script(ws, query)


@pytest.fixture(scope="module")
def server() -> Iterator[str]:
    with serve(
        handler,
        "127.0.0.1",
        0,
        select_subprotocol=lambda conn, subprotocols: subprotocols[0] if subprotocols else None,
    ) as srv:
        port = srv.socket.getsockname()[1]
        t = threading.Thread(target=srv.serve_forever, daemon=True)
        t.start()
        yield f"http://127.0.0.1:{port}"
        srv.shutdown()


@pytest.fixture(autouse=True)
def reset() -> Iterator[None]:
    SCRIPTS.clear()
    SEEN.clear()
    yield


def test_build_exec_url_uses_the_sprites_query_names() -> None:
    url = build_exec_url(
        "https://api.test",
        "/v1/machines/m/exec/stream",
        ["bash", "-c", "ls"],
        cwd="/app",
        env={"A": "1"},
        user="root",
        org="o",
    )
    parts = urlsplit(url)
    assert parts.scheme == "wss"
    q = parse_qs(parts.query)
    assert q["cmd"] == ["bash", "-c", "ls"] and q["path"] == ["bash"] and q["dir"] == ["/app"]
    assert q["env"] == ["A=1"] and q["user"] == ["root"] and q["org"] == ["o"]
    # Always present, never inferred.
    assert q["stdin"] == ["false"]
    tty = parse_qs(urlsplit(build_exec_url("http://x", "/p", ["sh"], tty=True, rows=40, cols=120)).query)
    assert tty["stdin"] == ["true"] and tty["tty"] == ["true"] and tty["rows"] == ["40"] and tty["cols"] == ["120"]


def test_frames_land_on_the_right_pipe_and_the_text_verdict_leads(server: str) -> None:
    def script(ws: Any, query: dict[str, list[str]]) -> None:
        ws.send(b"\x01hello ")
        ws.send(b"\x02warn\n")
        ws.send(b"\x01world\n")
        # The text verdict first, carrying a code one byte cannot: -1.
        ws.send(json.dumps({"type": "exit", "exit_code": -1}))
        ws.send(b"\x03\xff")

    SCRIPTS["echo"] = script
    c = PilotsClient("secret", base_url=server)
    stream = c.machines.exec_stream("m-1", ["echo", "hi"], cwd="/tmp")
    out, err, code = stream.output(timeout=5)
    assert out == b"hello world\n" and err == b"warn\n" and code == -1
    assert stream.exit_code == -1
    assert SEEN[-1]["subprotocol"] == "authorization.bearer.secret"
    assert SEEN[-1]["query"]["dir"] == ["/tmp"]


def test_stdin_is_off_unless_asked(server: str) -> None:
    got: list[bytes] = []

    def script(ws: Any, query: dict[str, list[str]]) -> None:
        if query["stdin"] == ["true"]:
            got.append(ws.recv())
            got.append(ws.recv())
        ws.send(b"\x03\x00")

    SCRIPTS["cat"] = script
    c = PilotsClient("k", base_url=server)
    quiet = c.machines.exec_stream("m-1", ["cat"])
    with pytest.raises(PilotsError, match="stdin=False"):
        quiet.write_stdin(b"x")
    assert quiet.wait(5) == 0

    loud = c.machines.exec_stream("m-1", ["cat"], stdin=True)
    loud.write_stdin("abc")
    loud.end_stdin()
    assert loud.wait(5) == 0
    assert got == [b"\x00abc", b"\x04"]


def test_a_close_without_an_exit_frame_is_an_error(server: str) -> None:
    def script(ws: Any, query: dict[str, list[str]]) -> None:
        ws.send(b"\x01partial")
        ws.close()

    SCRIPTS["drop"] = script
    c = PilotsClient("k", base_url=server)
    stream = c.machines.exec_stream("m-1", ["drop"])
    assert stream.stdout.read() == b"partial"
    with pytest.raises(PilotsError, match="before exit"):
        stream.wait(5)


def test_tty_resize_and_eot(server: str) -> None:
    got: list[Any] = []

    def script(ws: Any, query: dict[str, list[str]]) -> None:
        got.append(query.get("tty"))
        got.append(ws.recv())
        got.append(ws.recv())
        ws.send(b"\x03\x00")

    SCRIPTS["bash"] = script
    c = PilotsClient("k", base_url=server)
    term = c.machines.exec_stream("m-1", ["bash"], tty=True, rows=24, cols=80)
    term.resize(100, 30)
    term.end_stdin()
    assert term.wait(5) == 0
    assert got[0] == ["true"]
    assert json.loads(got[1]) == {"type": "resize", "cols": 100, "rows": 30}
    assert got[2] == b"\x04"
    plain = c.machines.exec_stream("m-1", ["bash"])
    with pytest.raises(PilotsError, match="without tty"):
        plain.resize(1, 1)
    plain.kill()


def test_connect_failure_is_a_pilots_error() -> None:
    with pytest.raises(PilotsError, match="could not connect"):
        ExecStream("ws://127.0.0.1:9/v1/x", "k", stdin=False, tty=False, open_timeout=1)
