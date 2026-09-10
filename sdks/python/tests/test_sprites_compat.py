"""The sprites-shaped face: the four rules that decide its shapes."""

from __future__ import annotations

import base64
import json
import threading
from collections.abc import Iterator
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from typing import Any

import pytest

from pilots.sprites_compat import Checkpoint, SpritesClient, URLSettings

STATE: dict[str, Any] = {}
SEEN: list[dict[str, Any]] = []


class Handler(BaseHTTPRequestHandler):
    def log_message(self, *args: Any) -> None:
        pass

    def _serve(self) -> None:
        length = int(self.headers.get("content-length") or 0)
        raw = self.rfile.read(length) if length else b""
        body = json.loads(raw) if raw else {}
        route = self.path.split("?")[0]
        SEEN.append({"method": self.command, "route": route, "body": body})
        status, payload = self._route(route, body)
        out = json.dumps(payload).encode() if payload is not None else b""
        self.send_response(status)
        self.send_header("content-type", "application/json")
        self.end_headers()
        self.wfile.write(out)

    def _route(self, route: str, body: dict[str, Any]) -> tuple[int, Any]:
        machines = STATE["machines"]
        if route == "/v1/machines" and self.command == "GET":
            return 200, list(machines.values())
        if route == "/v1/machines" and self.command == "POST":
            m = {
                "id": f"m-{len(machines) + 1}",
                "name": body.get("name", "unnamed"),
                "state": "running",
                "url": f"https://{body.get('name')}.pilotrun.app",
                "labels": body.get("labels"),
                "url_auth": body.get("url_auth", "public"),
            }
            machines[m["id"]] = m
            return 200, m
        for m in list(machines.values()):
            if route == f"/v1/machines/{m['id']}":
                if self.command == "DELETE":
                    machines.pop(m["id"])
                    return 204, None
                if self.command == "PATCH":
                    m.update(body)
                    return 200, m
                return 200, m
            if route == f"/v1/machines/{m['id']}/exec":
                return 200, STATE["exec"](body)
            if route == f"/v1/machines/{m['id']}/checkpoints":
                if self.command == "POST":
                    ck = {"id": f"ck-{len(STATE['checkpoints']) + 1}", "machine_id": m["id"], "seq": 1,
                          "comment": body.get("comment", ""), "durable": False, "created_at": 1750000000}
                    STATE["checkpoints"].append(ck)
                    return 200, ck
                return 200, STATE["checkpoints"]
        if route.startswith("/v1/checkpoints/") and route.endswith("/restore"):
            STATE["restored"] = route.split("/")[3]
            return 200, next(iter(machines.values()))
        return 404, {"error": "no such route", "code": "not_found"}

    do_GET = do_POST = do_PATCH = do_DELETE = _serve


@pytest.fixture(scope="module")
def server() -> Iterator[str]:
    srv = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
    threading.Thread(target=srv.serve_forever, daemon=True).start()
    yield f"http://127.0.0.1:{srv.server_port}"
    srv.shutdown()


@pytest.fixture(autouse=True)
def reset() -> Iterator[None]:
    STATE.clear()
    STATE.update(
        machines={},
        checkpoints=[],
        exec=lambda body: {"stdout": "", "stderr": "", "exit_code": 0},
        restored=None,
    )
    SEEN.clear()
    yield


def test_the_id_is_the_name_and_machine_id_carries_the_real_one(server: str) -> None:
    client = SpritesClient("k", base_url=server)
    sprite = client.create_sprite("demo")
    assert sprite.id == "demo" and sprite.name == "demo"
    assert sprite.machine_id.startswith("m-")
    assert sprite.url == "https://demo.pilotrun.app"
    # A lazy handle resolves by NAME.
    again = client.sprite("demo")
    assert again.machine_id == sprite.machine_id


def test_the_token_falls_back_to_the_pilots_variable(server: str, monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setenv("PILOT_API_KEY", "from-env")
    assert SpritesClient(base_url=server).client.api_key == "from-env"
    monkeypatch.delenv("PILOT_API_KEY")
    monkeypatch.setenv("SPRITES_TOKEN", "old-name")
    assert SpritesClient(base_url=server).client.api_key == "old-name"


def test_labels_and_url_settings_translate(server: str) -> None:
    client = SpritesClient("k", base_url=server)
    client.create_sprite("dev", labels=["team", "scratch"], url_settings=URLSettings(auth="sprite"))
    body = SEEN[-1]["body"]
    assert body["labels"] == {"team": "", "scratch": ""}
    assert body["url_auth"] == "org"


def test_restore_is_in_place_and_creates_nothing(server: str) -> None:
    client = SpritesClient("k", base_url=server)
    sprite = client.create_sprite("demo")
    before = sprite.machine_id
    ck = sprite.create_checkpoint("pre-upgrade")
    assert isinstance(ck, Checkpoint) and ck.id == "ck-1" and ck.comment == "pre-upgrade"
    creates = sum(1 for s in SEEN if s["route"] == "/v1/machines" and s["method"] == "POST")
    sprite.restore_checkpoint(ck.id)
    assert STATE["restored"] == ck.id
    assert sprite.machine_id == before
    assert sum(1 for s in SEEN if s["route"] == "/v1/machines" and s["method"] == "POST") == creates


def test_run_returns_a_completed_process(server: str) -> None:
    STATE["exec"] = lambda body: {"stdout": "hello\n", "stderr": "", "exit_code": 0}
    client = SpritesClient("k", base_url=server)
    sprite = client.create_sprite("demo")
    # `run` uses the stream; the buffered `exec` is the one this fake serves.
    res = sprite.exec("echo hello")
    assert res.stdout == "hello\n" and res.exit_code == 0


def test_the_filesystem_round_trips_through_base64(server: str) -> None:
    written: dict[str, str] = {}

    def fake_exec(body: dict[str, Any]) -> dict[str, Any]:
        cmd = body["cmd"]
        if "base64 -d" in cmd:
            encoded = cmd.split("printf %s '")[1].split("'")[0]
            written["body"] = base64.b64decode(encoded).decode()
            return {"stdout": "", "stderr": "", "exit_code": 0}
        if cmd.startswith("base64 -w0"):
            return {"stdout": base64.b64encode(b'{"debug": true}\n').decode(), "stderr": "", "exit_code": 0}
        if cmd.startswith("ls -1A"):
            return {"stdout": "a.txt\nb.txt\n", "stderr": "", "exit_code": 0}
        return {"stdout": "", "stderr": "", "exit_code": 0}

    STATE["exec"] = fake_exec
    client = SpritesClient("k", base_url=server)
    fs = client.create_sprite("demo").filesystem("/app")
    fs.write_text("config.json", '{"debug": true}\n')
    assert written["body"] == '{"debug": true}\n'
    assert fs.read_text("config.json") == '{"debug": true}\n'
    assert fs.listdir(".") == ["a.txt", "b.txt"]
    # The path is resolved against the working directory.
    assert "/app/config.json" in SEEN[-2]["body"]["cmd"]


def test_set_public_url_is_a_no_op(server: str) -> None:
    client = SpritesClient("k", base_url=server)
    sprite = client.create_sprite("demo")
    calls = len(SEEN)
    sprite.set_public_url()
    assert len(SEEN) == calls


def test_list_and_destroy(server: str) -> None:
    client = SpritesClient("k", base_url=server)
    client.create_sprite("alpha")
    client.create_sprite("beta")
    assert sorted(s.id for s in client.list_sprites()) == ["alpha", "beta"]
    assert [s.id for s in client.list_sprites(prefix="al")] == ["alpha"]
    client.destroy_sprite("alpha")
    assert [s.id for s in client.list_sprites()] == ["beta"]
