"""The drift test.

hostd's JSON tags are the contract. This SDK keeps a second copy of them,
which is exactly the situation where a copy rots silently: a field added to
Machine in Go would never reach a consumer and nothing would be red. So the
copy is checked against its source on every pytest run: parse every non-test
Go file in the two packages that own wire shapes, read the dataclasses in
``pilots.types``, and fail naming the struct and the tag when the two sides
disagree in either direction. Same rule, same regexes, as the JS SDK's
``test/drift.test.ts``.
"""

from __future__ import annotations

import dataclasses
import re
from pathlib import Path

import pytest

import pilots.types as types

REPO = Path(__file__).resolve().parents[3]
API_DIR = REPO / "apps" / "hostd" / "internal" / "api"
COMPOSE_DIR = REPO / "apps" / "hostd" / "internal" / "compose"


def strip_comments(src: str) -> str:
    src = re.sub(r"/\*[\s\S]*?\*/", "", src)
    return re.sub(r"//.*$", "", src, flags=re.M)


def go_structs(directory: Path, prefix: str = "") -> list[tuple[str, str, list[str]]]:
    """Every ``type X struct { ... }`` with its JSON tag names. gofmt puts the
    closing brace of a top-level struct at column 0, which is what makes the
    block boundary reliable without a real parser."""
    out = []
    for path in sorted(directory.glob("*.go")):
        if path.name.endswith("_test.go"):
            continue
        lines = strip_comments(path.read_text()).split("\n")
        i = 0
        while i < len(lines):
            m = re.match(r"^type ([A-Za-z0-9_]+) struct \{", lines[i])
            if not m:
                i += 1
                continue
            tags: list[str] = []
            i += 1
            while i < len(lines) and lines[i] != "}":
                t = re.search(r'json:"([^"]*)"', lines[i])
                if t:
                    name = t.group(1).split(",")[0]
                    if name and name != "-":
                        tags.append(name)
                i += 1
            if tags:
                out.append((prefix + m.group(1), path.name, tags))
    return out


def python_dataclasses() -> dict[str, list[str]]:
    out = {}
    for name in dir(types):
        obj = getattr(types, name)
        if isinstance(obj, type) and dataclasses.is_dataclass(obj) and obj.__module__ == types.__name__:
            out[name] = [f.name for f in dataclasses.fields(obj)]
    return out


@pytest.fixture(scope="module")
def structs() -> list[tuple[str, str, list[str]]]:
    if not API_DIR.exists():
        pytest.skip(f"{API_DIR} is not in this checkout")
    found = go_structs(API_DIR) + (go_structs(COMPOSE_DIR, "Compose") if COMPOSE_DIR.exists() else [])
    # A moved or renamed directory must fail loudly rather than pass
    # vacuously by finding nothing on either side.
    assert len(found) >= 20, f"only {len(found)} tagged structs under {API_DIR}; the directory moved?"
    return found


def test_types_mirror_every_wire_struct(structs: list[tuple[str, str, list[str]]]) -> None:
    mine = python_dataclasses()
    assert len(mine) >= 20
    problems = []
    for name, file, tags in structs:
        fields = mine.get(name)
        if fields is None:
            problems.append(f"pilots.types has no dataclass {name} (hostd {file}: {', '.join(tags)})")
            continue
        missing = [t for t in tags if t not in fields]
        extra = [f for f in fields if f not in tags]
        if missing or extra:
            problems.append(f"pilots.types {name} is missing {missing} / carries extra {extra} (hostd {file})")
    assert problems == []


def test_frame_constants_match_hostd(structs: list[tuple[str, str, list[str]]]) -> None:
    src = (API_DIR / "types.go").read_text()
    frames = re.findall(r"^\s*(Frame[A-Za-z]+)\s+byte\s*=\s*(\d+)$", src, flags=re.M)
    assert len(frames) >= 3, "no Frame constants found in hostd types.go"
    for name, value in frames:
        assert hasattr(types, name), f"pilots.types has no {name} (hostd says {value})"
        assert getattr(types, name) == int(value), f"{name} is {getattr(types, name)}, hostd says {value}"
