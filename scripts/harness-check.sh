#!/usr/bin/env bash
# Make the harnesses installed on THIS machine read back what `pilot mcp
# install` writes for them.
#
# Why this exists: `agents/harnesses.json` is a table of claims about ten
# third-party products, and nothing in CI can check a claim about somebody
# else's file format. The Go tests drive every row through our own writer and
# prove the file round-trips, which is a different question from "does the
# product accept it". A wrong path or a wrong key passes every test in the
# repository and fails in a user's hands.
#
# So this script asks the products themselves. It is NOT in CI, because it
# needs those binaries installed; run it when a row changes, and put the date
# in that row's `checked` field.
#
# It never touches your real configuration. Each harness is pointed at a
# scratch directory through its own environment variable, and the whole thing
# is removed on exit.
set -euo pipefail

cd "$(dirname "$0")/.."
ROOT="$PWD"

SCRATCH="$(mktemp -d "${TMPDIR:-/tmp}/pilots-harness-check.XXXXXX")"
trap 'rm -rf "$SCRATCH"' EXIT INT TERM

PILOT_BIN="${PILOT_BIN:-$SCRATCH/pilot}"
if [ ! -x "$PILOT_BIN" ]; then
  echo "building pilot"
  ( cd apps/pilot && go build -o "$PILOT_BIN" ./cmd/pilot )
fi

# A key that is obviously not real: the point is the SHAPE of the file, and a
# live token in a scratch config would be a live token in a temporary file.
export PILOT_API_KEY="pilot_harnesscheck000000000000000000000000000000000"
export PILOT_API_URL="${PILOT_API_URL:-https://api.pilotrun.app}"

pass=0
fail=0
skip=0

ok()   { printf '  ok    %-14s %s\n' "$1" "$2"; pass=$((pass + 1)); }
bad()  { printf '  FAIL  %-14s %s\n' "$1" "$2"; fail=$((fail + 1)); }
gone() { printf '  skip  %-14s %s\n' "$1" "$2"; skip=$((skip + 1)); }

have() { command -v "$1" >/dev/null 2>&1; }

echo "harness-check: asking each installed harness to read back what we wrote"
echo

# -- Codex -------------------------------------------------------------------
# CODEX_HOME redirects its whole configuration, and `codex mcp list` prints
# what it parsed, including whether it understood the auth.
if have codex; then
  export CODEX_HOME="$SCRATCH/codex"
  mkdir -p "$CODEX_HOME"
  # The writer targets ~/.codex/config.toml, so HOME is pointed here too.
  if HOME="$SCRATCH/codex-home" "$PILOT_BIN" mcp install codex >/dev/null 2>&1; then
    cp "$SCRATCH/codex-home/.codex/config.toml" "$CODEX_HOME/config.toml"
    if out="$(codex mcp list 2>&1)" && printf '%s' "$out" | grep -q pilots; then
      if printf '%s' "$out" | grep -qi 'bearer'; then
        ok codex "parsed it, and recognised the bearer token"
      else
        bad codex "listed the server but not its auth: $(printf '%s' "$out" | tr '\n' ' ')"
      fi
    else
      bad codex "did not list the server: $(printf '%s' "$out" | tr '\n' ' ')"
    fi
  else
    bad codex "pilot mcp install codex failed"
  fi
  unset CODEX_HOME
else
  gone codex "not installed"
fi

# -- OpenCode ----------------------------------------------------------------
# It resolves opencode.json from the working directory, so the check runs
# inside the scratch directory rather than redirecting HOME.
if have opencode; then
  mkdir -p "$SCRATCH/oc"
  if ( cd "$SCRATCH/oc" && "$PILOT_BIN" mcp install opencode --project >/dev/null 2>&1 ); then
    if out="$( cd "$SCRATCH/oc" && opencode mcp list 2>&1 )" && printf '%s' "$out" | grep -q pilots; then
      ok opencode "parsed it from opencode.json"
    else
      bad opencode "did not list the server: $(printf '%s' "$out" | tr '\n' ' ')"
    fi
  else
    bad opencode "pilot mcp install opencode --project failed"
  fi
else
  gone opencode "not installed"
fi

# -- The rest ----------------------------------------------------------------
# Claude Code, Gemini, VS Code, Cursor, Windsurf, Zed and Claude Desktop have
# no command that parses a config and reports what it found without starting
# an interactive session, so the check is the weaker one their absence allows:
# the file we write is valid in its own format and holds the entry at the path
# the row claims. That is what the Go tests already assert, so rather than
# repeat them, print the entry for a person to compare against the row's
# `source` URL.
for h in claude-code cursor vscode gemini zed windsurf claude-desktop; do
  # A harness with no hosted form refuses `--print` without `--stdio`, and
  # that refusal is correct rather than a failure: Claude Desktop takes a
  # local command and nothing else. Print whichever form that row HAS.
  if entry="$("$PILOT_BIN" mcp install "$h" --print 2>/dev/null)"; then
    form=hosted
  elif entry="$("$PILOT_BIN" mcp install "$h" --print --stdio 2>&1)"; then
    form=stdio
  else
    bad "$h" "--print failed both ways: $entry"
    continue
  fi
  printf '  read  %-14s %s, compare against the row source:\n' "$h" "$form"
  printf '%s\n' "$entry" | sed 's/^/          /'
done

echo
printf 'harness-check: %d parsed, %d failed, %d not installed\n' "$pass" "$fail" "$skip"
echo "sources: $ROOT/agents/harnesses.json"
[ "$fail" -eq 0 ]
