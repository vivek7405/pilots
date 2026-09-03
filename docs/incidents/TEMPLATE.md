# YYYY-MM-DD — <one-line title>

## Summary

One paragraph. **Customer-visible impact first**: who saw what, for how long,
and on which surface. Then, in a sentence, what caused it. If nobody saw
anything, say that in the first sentence too — a self-heal that worked is
still worth the entry, and "no customer impact" is a finding, not an excuse to
skip the rest of the form.

## Timeline

UTC, `hh:mm`, one line per event. Include the events that were NOT noticed at
the time — that gap between "it started" and "we saw it" is usually the most
actionable number in the whole document.

| Time (UTC) | Event |
|---|---|
| hh:mm | |
| hh:mm | |

## Detection

What noticed it, exactly: a battery step and its name, a customer report, a
log line (quote it), an alert, or a person looking at a graph. If the answer
is "a person happened to look", write that — it is the finding.

## Root cause

The change, condition or interaction that caused it. Not the symptom. If the
cause was a change of ours, name the commit or PR. If it is still unknown,
write "unknown" and say what would settle it; do not write a plausible story.

## Blast radius

| | |
|---|---|
| Hosts affected | |
| Machines affected | |
| Organisations affected | |
| Data lost | |

## What was measured

Numbers with units, from the run or from the host. `n/a` where an operation
was not exercised — never a blank.

| Metric | Value | SLO |
|---|---|---|
| create p50 | | < 500 ms |
| wake p50 | | < 200 ms |
| checkpoint resume gap p50 | | < 500 ms |
| release restore p50 | | < 1 s |
| promote p50 | | < 1.5 s |
| re-seed convergence | | < 60 s |
| corrosion egress (`systemctl show corrosion -p IPEgressBytes`) | | |
| corrosion memory (`systemctl show corrosion -p MemoryCurrent`) | | |

## Recovery steps run

The exact commands, in order, including the ones that did not work. A runbook
is written by copying this section of a real incident; a paraphrase cannot be.

```
```

## Follow-ups

Each one owned and dated. No orphan bullets.

- [ ] <what> — @owner, by YYYY-MM-DD

## What we would tell a customer

Two or three sentences, in the words we would actually send. Writing it here
is what stops the first draft being written under time pressure with a
customer waiting.
