# The small-model demo

The scripted gate proves the platform answers correctly. It cannot prove the
answers are *usable*, because the script already knows what to call. This is
the gate for that: a small local model, given only the skill and `pilot mcp`,
takes a directory to a URL.

It is a manual gate, run before the issue closes and re-run whenever the tool
descriptions or the skill change. A failed run is a description bug or a skill
bug. It is never a reason to reach for a bigger model: the whole point of the
bar in `ARCHITECTURE.md` is that the surface works for a model this size, and
swapping the model would hide exactly the defect the run exists to find.

## The model

`qwen3:8b-q4_K_M`, through Ollama. 5.2 GB, Apache-2.0, `tools` capability in
Ollama's library, inside the 7B to 9B band, fits 8 GB of VRAM.

Pinned, and not because it is the best available. It is representative of what
somebody will actually point at this on a laptop, and a moving target would
make a failed run ambiguous between "the skill regressed" and "the model
changed".

## The setup

```
ollama pull qwen3:8b-q4_K_M
```

`mcphost` drives it, because it speaks Ollama and launches stdio MCP servers
from a JSON config, so no code is written for the demo:

```json
{
  "mcpServers": {
    "pilots": {
      "command": "pilot",
      "args": ["mcp"],
      "env": { "PILOT_API_KEY": "pilot_...", "PILOT_API_URL": "http://<host>:8080" }
    }
  }
}
```

```
mcphost --config ./mcphost.json \
  --model ollama:qwen3:8b-q4_K_M \
  --system-prompt-file agents/skills/pilots/SKILL.md
```

## The prompt

Exactly this, and nothing else:

```
Deploy packages/cli/test/fixtures/webjs-app to pilots and give me the URL.
```

No hint about `init`, no hint about `deploy`, no mention of a Dockerfile. If
the model needs one, the skill is not doing its job.

## What passing means

The model calls `init`, then `deploy` with `dir`, and reports the URL. Nothing
else. In particular:

- it does not call `generate_dockerfile` first, because `deploy` already does
  that and both the tool description and the deploy reference say so;
- it does not read the repository to work out the framework, because rule 6 of
  the skill says the platform's answer is complete;
- it does not invent a path, because rule 2 says to ask.

Any other path is a bug in a description or in the skill. Fix it, re-run, and
record the fix below under a dated heading.

## Capturing a run

```
script -q -c 'mcphost --config ./mcphost.json --model ollama:qwen3:8b-q4_K_M \
  --system-prompt-file agents/skills/pilots/SKILL.md' /tmp/agent-demo.log
```

Attach the transcript to the pull request that closes the issue.

## Runs

*(A dated heading per run, with the transcript's outcome and any fix it
forced. The first entry is added when the run is performed against a
Firecracker host: this repository's checks run on machines with no KVM, so the
run is not something CI can do.)*
