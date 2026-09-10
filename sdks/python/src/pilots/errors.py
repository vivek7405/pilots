"""The error model.

Every non-2xx becomes a ``PilotsError``, and the cases a caller has to branch
on -- a missing machine, a quota refusal, a compose file the planner cannot
express, a build that failed after the status line was already 200 -- become
subclasses carrying the fields needed to act, so nobody re-parses a body
string to find out what happened.

``code``, ``next`` and ``details`` are on the base class rather than only on
the subclasses: a caller that does not branch still wants to print the next
step, and a code this SDK version has never heard of must still reach it.
"""

from __future__ import annotations

from typing import Any


class PilotsError(Exception):
    """Base class for everything this SDK raises."""

    def __init__(
        self,
        message: str,
        *,
        status: int = 0,
        body: str = "",
        code: str = "",
        next: str = "",
        details: Any = None,
    ) -> None:
        super().__init__(message)
        self.message = message
        #: HTTP status, or 0 for an error raised before a request was made.
        self.status = status
        #: The response body, verbatim, for anything the fields did not capture.
        self.body = body
        #: The body's stable ``code``. Empty when the server sent none.
        self.code = code
        #: The body's ``next``: the one thing to do about it.
        self.next = next
        #: The body's ``details``, typed per code.
        self.details = details

    def __str__(self) -> str:
        if self.next:
            return f"{self.message} (next: {self.next})"
        return self.message


class NotFoundError(PilotsError):
    """404. The machine, checkpoint, service, volume or build does not exist."""

    def __init__(self, message: str, **init: Any) -> None:
        init.setdefault("status", 404)
        super().__init__(message, **init)


class QuotaExceededError(PilotsError):
    """429. The org (or, for builds, the host) is at its ceiling.

    ``quota`` names which one, so a caller raises the right limit rather than
    guessing from a sentence.
    """

    def __init__(
        self, message: str, *, quota: str, limit: int, used: int, scope: str | None = None, **init: Any
    ) -> None:
        init.setdefault("status", 429)
        super().__init__(message, **init)
        self.quota = quota
        self.limit = limit
        self.used = used
        #: "host" when the ceiling is the host's rather than the org's.
        self.scope = scope


class ComposePlanError(PilotsError):
    """400 from ``POST /v1/compose/plan`` listing what the planner will not accept.

    ``unsupported`` is a list of ``{service, key, message}``.
    """

    def __init__(self, error: str, unsupported: list[dict[str, Any]], **init: Any) -> None:
        detail = "; ".join(f"{u.get('service')}.{u.get('key')}: {u.get('message')}" for u in unsupported)
        init.setdefault("status", 400)
        super().__init__(f"{error}: {detail}" if detail else error, **init)
        self.error = error
        self.unsupported = unsupported


class BuildFailedError(PilotsError):
    """A build that failed.

    The status code is 200: hostd decides it before the build's outcome is
    known, so a client can watch a ten-minute build instead of waiting for it.
    The LAST NDJSON line is the verdict, and this is what ``result()`` raises
    when that line carries ``error``. It keeps every line so an agent can read
    the failing step and patch the Dockerfile.
    """

    def __init__(self, message: str, build_id: str, lines: list[Any]) -> None:
        super().__init__(message, status=200)
        self.build_id = build_id
        self.lines = lines


class HealthGateError(PilotsError):
    """422 ``health_gate_failed``: the release started and never answered its
    health check inside the grace period. Matched by ``code``, never by the
    status alone."""

    def __init__(self, message: str, details: Any, **init: Any) -> None:
        init.setdefault("status", 422)
        init["details"] = details
        super().__init__(message, **init)


class UnknownFrameworkError(PilotsError):
    """400 ``unknown_framework``: no compose file, no Dockerfile and no
    framework the platform recognises. ``details`` carries the listing, the
    manifests and the two Dockerfile rules, which is enough to write one."""

    def __init__(self, message: str, details: Any, **init: Any) -> None:
        init.setdefault("status", 400)
        init["details"] = details
        super().__init__(message, **init)
