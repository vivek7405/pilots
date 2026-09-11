"""The transport: one bearer-authenticated httpx client, one status-to-error
map, and an NDJSON line iterator that yields as the bytes arrive.

Nothing here retries, pools beyond httpx's own defaults, or rate-limits. A
caller who wants any of those passes their own ``httpx.Client``.
"""

from __future__ import annotations

import json
import os
from collections.abc import Iterator
from typing import Any

import httpx

from .errors import (
    ComposePlanError,
    HealthGateError,
    NotFoundError,
    PilotsError,
    QuotaExceededError,
    UnknownFrameworkError,
)

DEFAULT_BASE_URL = "https://api.pilotrun.app"
DEFAULT_TIMEOUT = 30.0


def resolve_base_url(explicit: str | None = None) -> str:
    """The base URL a client was given, then ``PILOT_API_URL``, then the default."""
    return (explicit or os.environ.get("PILOT_API_URL") or DEFAULT_BASE_URL).rstrip("/")


class Http:
    def __init__(
        self,
        api_key: str,
        *,
        base_url: str | None = None,
        timeout: float = DEFAULT_TIMEOUT,
        org: str | None = None,
        client: httpx.Client | None = None,
    ) -> None:
        if not api_key:
            # Before any request: a client built with no key would otherwise
            # fail once per call with a 401 that says nothing about the cause.
            raise PilotsError("an API key is required: PilotsClient(api_key=os.environ['PILOT_API_KEY'])")
        self.api_key = api_key
        self.base_url = resolve_base_url(base_url)
        self.timeout = timeout
        #: Makes an ADMIN key act as one org: every request carries ``?org=``.
        self.org = org or None
        self._client = client or httpx.Client()
        self._owns_client = client is None

    def close(self) -> None:
        if self._owns_client:
            self._client.close()

    def url(self, path: str) -> str:
        return self.base_url + path

    def _params(self, query: dict[str, Any] | None) -> dict[str, Any]:
        params = {k: v for k, v in (query or {}).items() if v is not None}
        # The org narrowing is applied here so it reaches every route: a
        # client acting as an org must create as it, be charged as it and
        # read as it.
        if self.org:
            params["org"] = self.org
        return params

    def send(
        self,
        method: str,
        path: str,
        *,
        json_body: Any = None,
        content: bytes | Iterator[bytes] | None = None,
        content_type: str | None = None,
        query: dict[str, Any] | None = None,
        timeout: float | None | object = DEFAULT_TIMEOUT,
        stream: bool = False,
    ) -> httpx.Response:
        """Performs the request and raises on any non-2xx.

        ``timeout=None`` disables the client deadline; builds, log follows and
        deploys use it, because they outlive any reasonable number.
        """
        headers: dict[str, str] = {"authorization": f"Bearer {self.api_key}"}
        if content is not None and content_type:
            headers["content-type"] = content_type
        deadline: float | None
        if timeout is DEFAULT_TIMEOUT:
            deadline = self.timeout
        else:
            deadline = timeout  # type: ignore[assignment]
        req = self._client.build_request(
            method,
            self.url(path),
            headers=headers,
            params=self._params(query),
            json=json_body,
            content=content,
            timeout=deadline,
        )
        try:
            res = self._client.send(req, stream=stream)
        except httpx.HTTPError as cause:
            raise PilotsError(f"{method} {path}: {cause}") from cause
        if res.status_code >= 400:
            if stream:
                res.read()
            raise to_error(res, method, path)
        return res

    def json(self, method: str, path: str, **kw: Any) -> Any:
        res = self.send(method, path, **kw)
        if not res.content:
            return None
        return res.json()

    def text(self, method: str, path: str, **kw: Any) -> str:
        return self.send(method, path, **kw).text

    def none(self, method: str, path: str, **kw: Any) -> None:
        self.send(method, path, **kw)


def to_error(res: httpx.Response, method: str, path: str) -> PilotsError:
    """Maps a failed response onto the narrowest error class that fits it."""
    body = res.text
    try:
        parsed = json.loads(body) if body else {}
    except ValueError:
        parsed = {}
    record: dict[str, Any] = parsed if isinstance(parsed, dict) else {}
    message = record.get("error") if isinstance(record.get("error"), str) and record.get("error") else None
    message = message or f"{method} {path} failed with {res.status_code}"
    init: dict[str, Any] = {"body": body}
    if isinstance(record.get("code"), str):
        init["code"] = record["code"]
    if isinstance(record.get("next"), str):
        init["next"] = record["next"]
    if "details" in record:
        init["details"] = record["details"]

    if res.status_code == 404:
        return NotFoundError(message, **init)
    if res.status_code == 429 and isinstance(record.get("quota"), str):
        return QuotaExceededError(
            message,
            quota=record["quota"],
            limit=int(record.get("limit", 0)),
            used=int(record.get("used", 0)),
            scope=record.get("scope"),
            **init,
        )
    if res.status_code == 400 and isinstance(record.get("unsupported"), list):
        return ComposePlanError(str(record.get("error", "")), record["unsupported"], **init)
    # Matched on the code and never on the status: 422 is the shape of this
    # one answer today, and a later 422 for something else must not land here.
    if record.get("code") == "health_gate_failed":
        return HealthGateError(message, init.pop("details", None), **init)
    if record.get("code") == "unknown_framework":
        return UnknownFrameworkError(message, init.pop("details", None), **init)
    return PilotsError(message, status=res.status_code, **init)


def text_lines(res: httpx.Response) -> Iterator[str]:
    """Yields each non-blank line of a streamed body as it arrives. Only the
    line terminator is stripped, never the line's own whitespace: a trim would
    flatten the indentation of every stack trace a guest prints."""
    try:
        for line in res.iter_lines():
            if line.endswith("\r"):
                line = line[:-1]
            if line.strip():
                yield line
    finally:
        res.close()


def ndjson(res: httpx.Response) -> Iterator[dict[str, Any]]:
    """Yields each NDJSON line as it arrives; never buffers the whole body."""
    for line in text_lines(res):
        yield json.loads(line)
