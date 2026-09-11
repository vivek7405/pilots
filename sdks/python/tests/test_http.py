"""The HTTP layer against an in-process fake hostd: auth header, org
narrowing, the error map, NDJSON streaming and the build verdict."""

from __future__ import annotations

import json
import threading
from collections.abc import Iterator
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from typing import Any

import pytest

import pilots
from pilots import (
    BuildFailedError,
    ComposePlanError,
    HealthGateError,
    NotFoundError,
    PilotsClient,
    PilotsError,
    QuotaExceededError,
    UnknownFrameworkError,
)

# path -> (status, body bytes or callable(handler) -> (status, headers, body))
ROUTES: dict[str, Any] = {}
SEEN: list[dict[str, Any]] = []


class Handler(BaseHTTPRequestHandler):
    def log_message(self, *args: Any) -> None:  # quiet
        pass

    def _serve(self) -> None:
        length = int(self.headers.get("content-length") or 0)
        body = self.rfile.read(length) if length else b""
        SEEN.append(
            {
                "method": self.command,
                "path": self.path,
                "auth": self.headers.get("authorization"),
                "content_type": self.headers.get("content-type"),
                "body": body,
            }
        )
        route = self.path.split("?")[0]
        entry = ROUTES.get(f"{self.command} {route}")
        if entry is None:
            self.send_response(404)
            self.send_header("content-type", "application/json")
            self.end_headers()
            self.wfile.write(json.dumps({"error": "no such route", "code": "not_found", "next": "check the path"}).encode())
            return
        status, headers, payload = entry(self) if callable(entry) else entry
        self.send_response(status)
        for k, v in headers.items():
            self.send_header(k, v)
        self.end_headers()
        if isinstance(payload, (bytes, bytearray)):
            self.wfile.write(payload)
        else:
            for chunk in payload:
                self.wfile.write(chunk)
                self.wfile.flush()

    do_GET = do_POST = do_PATCH = do_DELETE = do_PUT = _serve


def j(status: int, obj: Any, **headers: str) -> tuple[int, dict[str, str], bytes]:
    return status, {"content-type": "application/json", **headers}, json.dumps(obj).encode()


@pytest.fixture(scope="module")
def server() -> Iterator[str]:
    srv = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
    t = threading.Thread(target=srv.serve_forever, daemon=True)
    t.start()
    yield f"http://127.0.0.1:{srv.server_port}"
    srv.shutdown()


@pytest.fixture(autouse=True)
def reset() -> Iterator[None]:
    ROUTES.clear()
    SEEN.clear()
    yield


def test_an_empty_key_is_refused_before_any_request(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.delenv("PILOT_API_KEY", raising=False)
    with pytest.raises(PilotsError, match="API key"):
        PilotsClient("")


def test_base_url_falls_back_to_the_environment(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setenv("PILOT_API_URL", "http://fleet.test:8080/")
    assert PilotsClient("k").base_url == "http://fleet.test:8080"
    monkeypatch.delenv("PILOT_API_URL")
    assert PilotsClient("k").base_url == pilots.DEFAULT_BASE_URL
    assert PilotsClient("k", base_url="http://x/").base_url == "http://x"


def test_bearer_and_org_reach_every_call(server: str) -> None:
    ROUTES["GET /v1/machines"] = j(
        200, [{"id": "m-1", "name": "demo", "state": "running", "knobs": {"auto_stop": "suspend"}}]
    )
    c = PilotsClient("secret", base_url=server, org="acme")
    machines = c.machines.list()
    assert [m.id for m in machines] == ["m-1"]
    assert machines[0].knobs.auto_stop == "suspend"
    assert SEEN[-1]["auth"] == "Bearer secret"
    assert "org=acme" in SEEN[-1]["path"]


def test_requests_drop_none_fields(server: str) -> None:
    ROUTES["POST /v1/machines"] = j(200, {"id": "m-2", "name": "x", "url": "https://x"})
    c = PilotsClient("k", base_url=server)
    m = c.machines.create(pilots.CreateMachineRequest(name="x", vcpus=2))
    assert m.id == "m-2"
    assert json.loads(SEEN[-1]["body"]) == {"name": "x", "vcpus": 2}
    m = c.machines.create(name="y", labels={"a": "b"})
    assert json.loads(SEEN[-1]["body"]) == {"name": "y", "labels": {"a": "b"}}


def test_the_error_map(server: str) -> None:
    c = PilotsClient("k", base_url=server)
    with pytest.raises(NotFoundError) as nf:
        c.machines.get("nope")
    assert nf.value.status == 404 and nf.value.code == "not_found" and nf.value.next == "check the path"

    ROUTES["POST /v1/services"] = j(
        429, {"error": "too many", "code": "quota_exceeded", "next": "raise it", "quota": "machines", "limit": 3, "used": 3}
    )
    with pytest.raises(QuotaExceededError) as qe:
        c.services.create({"name": "s"})
    assert (qe.value.quota, qe.value.limit, qe.value.used, qe.value.scope) == ("machines", 3, 3, None)

    ROUTES["POST /v1/compose/plan"] = j(
        400,
        {
            "error": "unsupported",
            "code": "plan_unsupported",
            "next": "fix",
            "unsupported": [{"service": "db", "key": "privileged", "message": "no"}],
        },
    )
    with pytest.raises(ComposePlanError) as cp:
        c.compose.plan({"compose": "services: {}"})
    assert cp.value.unsupported[0]["key"] == "privileged"
    assert "db.privileged: no" in str(cp.value)

    ROUTES["POST /v1/services/s1/deploy"] = j(
        422,
        {
            "error": "never healthy",
            "code": "health_gate_failed",
            "next": "diagnose",
            "details": {
                "service": "s1",
                "replica": "m-9",
                "release": "r",
                "grace_sec": 30,
                "last": {"error": "refused"},
            },
        },
    )
    with pytest.raises(HealthGateError) as hg:
        c.services.deploy("s1")
    assert hg.value.details["replica"] == "m-9"

    ROUTES["POST /v1/plan"] = j(
        400,
        {
            "error": "unknown",
            "code": "unknown_framework",
            "next": "write a Dockerfile",
            "details": {"dir": ".", "rules": ["bind 0.0.0.0"]},
        },
    )
    with pytest.raises(UnknownFrameworkError) as uf:
        c.plan(b"tar")
    assert uf.value.details["rules"] == ["bind 0.0.0.0"]
    assert SEEN[-1]["content_type"] == "application/x-tar"

    # An unknown code still reaches the caller with its next step.
    ROUTES["GET /v1/hosts"] = j(418, {"error": "teapot", "code": "brand_new", "next": "upgrade the sdk"})
    with pytest.raises(PilotsError) as pe:
        c.hosts.list()
    assert (pe.value.status, pe.value.code, pe.value.next) == (418, "brand_new", "upgrade the sdk")


def test_build_stream_reads_the_verdict_from_the_last_line(server: str) -> None:
    lines = [
        {"step": "1/3", "line": "FROM node", "ts": 1},
        {"step": "2/3", "line": "RUN npm ci", "ts": 2},
        {"ts": 3, "result": "rootfs-abc", "release": "rel-1"},
    ]
    ROUTES["POST /v1/builds"] = (
        200,
        {"content-type": "application/x-ndjson", "x-pilot-build-id": "b-1"},
        [(json.dumps(line) + "\n").encode() for line in lines],
    )
    c = PilotsClient("k", base_url=server)
    build = c.builds.create(b"tar", deploy="svc")
    assert build.build_id == "b-1"
    assert "deploy=svc" in SEEN[-1]["path"]
    assert build.result() == "rootfs-abc"
    assert build.release == "rel-1"
    assert [ln.step for ln in build.lines[:2]] == ["1/3", "2/3"]

    ROUTES["POST /v1/builds"] = (
        200,
        {"content-type": "application/x-ndjson", "x-pilot-build-id": "b-2"},
        [b'{"step":"1/1","line":"boom","ts":1}\n', b'{"ts":2,"error":"npm ci failed","code":"build_failed"}\n'],
    )
    with pytest.raises(BuildFailedError) as bf:
        c.builds.create(b"tar").result()
    assert bf.value.build_id == "b-2" and len(bf.value.lines) == 2

    ROUTES["POST /v1/builds"] = (200, {"content-type": "application/x-ndjson", "x-pilot-build-id": "b-3"}, [b""])
    with pytest.raises(BuildFailedError, match="without a verdict"):
        c.builds.create(b"tar").result()


def test_follow_logs_streams_lines_verbatim(server: str) -> None:
    ROUTES["GET /v1/machines/m-1/logs"] = (
        200,
        {"content-type": "text/plain"},
        [b"boot\n", b"  indented trace\r\n", b"\n", b"done"],
    )
    c = PilotsClient("k", base_url=server)
    assert list(c.machines.follow_logs("m-1")) == ["boot", "  indented trace", "done"]
    assert "follow=1" in SEEN[-1]["path"]


def test_exec_extends_the_deadline_for_a_long_timeout(server: str) -> None:
    ROUTES["POST /v1/machines/m-1/exec"] = j(200, {"stdout": "hi\n", "stderr": "", "exit_code": 0})
    c = PilotsClient("k", base_url=server, timeout=1)
    res = c.machines.exec("m-1", cmd="echo hi", timeout_ms=120_000)
    assert res.stdout == "hi\n" and res.exit_code == 0
    assert json.loads(SEEN[-1]["body"]) == {"cmd": "echo hi", "timeout_ms": 120000}


def test_ids_are_path_encoded(server: str) -> None:
    ROUTES["DELETE /v1/domains/shop.example.com"] = (204, {}, b"")
    c = PilotsClient("k", base_url=server)
    c.domains.remove("shop.example.com")
    assert SEEN[-1]["path"] == "/v1/domains/shop.example.com"
    ROUTES["GET /v1/machines/a%2Fb"] = j(200, {"id": "a/b"})
    assert c.machines.get("a/b").id == "a/b"
