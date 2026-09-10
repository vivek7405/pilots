"""The build log stream.

``POST /v1/builds`` answers 200 before the build starts, because a client
watching a ten-minute build needs the first step's output in the first
second. The consequence is that the status code cannot be the verdict: the
LAST NDJSON line is, and a line carrying ``error`` means the build failed
under a 200.
"""

from __future__ import annotations

from collections.abc import Iterator

import httpx

from ._http import ndjson
from .errors import BuildFailedError
from .types import BuildLogLine, from_json


class BuildStream:
    def __init__(self, res: httpx.Response, build_id: str | None = None) -> None:
        #: Also in the ``X-Pilot-Build-Id`` header, so a lost connection can reattach.
        self.build_id = build_id or res.headers.get("x-pilot-build-id", "")
        #: Every line seen so far, in order.
        self.lines: list[BuildLogLine] = []
        self._res = res
        self._source = ndjson(res)
        self._closed = False

    def __iter__(self) -> Iterator[BuildLogLine]:
        """``for line in build``. Consumes the stream; iterate once."""
        for raw in self._source:
            line = from_json(BuildLogLine, raw)
            self.lines.append(line)
            yield line

    def close(self) -> None:
        """Stops reading and releases the response. A ``result()`` after this
        raises rather than hangs: an interrupted build must never read as a
        successful one."""
        self._closed = True
        self._res.close()

    @property
    def release(self) -> str | None:
        """The deployment this build was cut into, once the stream has been
        read. Present only on a build whose request named a service to deploy."""
        return self.lines[-1].release if self.lines else None

    def result(self) -> str:
        """Drains the stream and returns the rootfs build id.

        Raises ``BuildFailedError`` when the last line carries ``error``, and
        equally when the stream ended with no verdict at all.
        """
        if not self._closed:
            for _ in self:
                pass
        last = self.lines[-1] if self.lines else None
        if last is not None and last.error:
            raise BuildFailedError(last.error, self.build_id, self.lines)
        if last is not None and last.result:
            return last.result
        raise BuildFailedError("the build stream ended without a verdict", self.build_id, self.lines)

    def __enter__(self) -> BuildStream:
        return self

    def __exit__(self, *exc: object) -> None:
        self.close()
