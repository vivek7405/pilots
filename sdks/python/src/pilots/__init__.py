# ruff: noqa: I001
"""pilots: instant sandboxes and durable production services on one
primitive, Firecracker microVMs.

    from pilots import PilotsClient

    pilots = PilotsClient()  # PILOT_API_KEY, PILOT_API_URL
    m = pilots.machines.create(name="demo")
    print(m.url)
    print(pilots.machines.exec(m.id, cmd="uname -a").stdout)
"""

# Every wire type, by hostd's name, first: the exception classes below shadow
# the one wire shape that shares a name (ComposePlanError), because a caller
# catches the exception and decodes the body through it.
from .types import *  # noqa: F401,F403
from .types import from_json, to_json

from ._http import DEFAULT_BASE_URL, DEFAULT_TIMEOUT
from .builds import BuildStream
from .client import (
    APIKeys,
    Builds,
    Checkpoints,
    Compose,
    Domains,
    Hosts,
    Machines,
    PilotsClient,
    Quotas,
    Repos,
    Services,
    Usage,
    Volumes,
)
from .errors import (  # type: ignore[assignment]  # ComposePlanError: the exception, not the wire shape
    BuildFailedError,
    ComposePlanError,
    HealthGateError,
    NotFoundError,
    PilotsError,
    QuotaExceededError,
    UnknownFrameworkError,
)
from .stream import ExecStream, build_exec_url

__version__ = "0.1.0"

__all__ = [
    "PilotsClient",
    "Machines",
    "Builds",
    "Checkpoints",
    "Services",
    "Domains",
    "Volumes",
    "Hosts",
    "APIKeys",
    "Repos",
    "Quotas",
    "Usage",
    "Compose",
    "BuildStream",
    "ExecStream",
    "build_exec_url",
    "PilotsError",
    "NotFoundError",
    "QuotaExceededError",
    "ComposePlanError",
    "BuildFailedError",
    "HealthGateError",
    "UnknownFrameworkError",
    "from_json",
    "to_json",
    "DEFAULT_BASE_URL",
    "DEFAULT_TIMEOUT",
]
