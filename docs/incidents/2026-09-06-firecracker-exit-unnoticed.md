# 2026-09-06 — hostd never noticed a Firecracker exit; a one-replica service answered 502 for two hours

## Summary

The webjs website, a one-replica service on the single-host laptop rig,
answered `502 machine unreachable` on every request for two hours and six
minutes, from 11:00 to 13:06 UTC, until an operator rolled a new replica and
deleted the old one by hand. The host had been suspended and resumed; its
Firecracker did not survive the resume and became a zombie, and hostd had no
code path that notices a Firecracker exiting on its own. The row stayed
`running`, the router kept proxying to a guest that did not exist, and the idle
monitor and the autoscaler retried a suspend against the corpse every ten
seconds. No data was lost; the machine's last suspend image was intact.

## Timeline

| Time (UTC) | Event |
|---|---|
| 04:33 | `woke machine on request` for the website's replica; normal service |
| 11:00:05 | host suspend/resume (`PM: suspend exit`); the Firecracker is a zombie, `nbd.sock` gone, `uffd-handler` still alive |
| 11:00:07 | first symptom in the log: `the guest's sync failed before snapshotting`, then `could not scale a service … nbd.sock: connection refused` |
| 11:00:15 onward | `snapshot failed and the guest could not be resumed; it is frozen and will not answer`, every 10 s, 82 times |
| 11:00 to 13:06 | every request to the service URL answers 502; `GET /v1/machines/{id}` says `running` |
| 13:04 | operator runs `POST /v1/services/{id}/deploy {build}`; a new replica is healthy in 12 s; the rollout logs `could not suspend a superseded replica` for the dead one |
| 13:06 | operator runs `DELETE /v1/machines/{id}`; the zombie is reaped and the retry loop stops |

## Detection

A person looked at the site and got a 502, then read `hostd`'s log. No battery
step, no alert. `pilots_machines{state="running"}` counted the dead machine as
running the whole time.

## Root cause

hostd spawned each Firecracker with `cmd.Start()` and never waited on it except
in `Kill`. `processAlive` was `kill(pid, 0)`, which succeeds on a zombie. So an
exit hostd did not cause was invisible to every loop, all of which key on
`row.State == "running"`. Fixed by #78 (reap-and-react: a watcher per child, a
pidfd per adopted process, `onExit` in the machine manager).

## Blast radius

| | |
|---|---|
| Hosts affected | 1 (the laptop rig) |
| Machines affected | 1 |
| Organisations affected | 1 |
| Data lost | none; the last suspend image restored on the next wake after the fix |

## What was measured

| Metric | Value | SLO |
|---|---|---|
| create p50 | n/a | < 500 ms |
| wake p50 | n/a | < 200 ms |
| checkpoint resume gap p50 | n/a | < 500 ms |
| release restore p50 | 12 s to healthy (the manual deploy) | < 1 s |
| promote p50 | n/a | < 1.5 s |
| re-seed convergence | n/a | < 60 s |
| corrosion egress | n/a (sqlite host) | |
| corrosion memory | n/a (sqlite host) | |
| time to detection | 2 h 4 min | |

## Recovery steps run

```
curl -X POST -H "Authorization: Bearer $KEY" http://api.pilots.localhost:8080/v1/services/$SVC/deploy -d '{"build":"..."}'
curl -X DELETE -H "Authorization: Bearer $KEY" http://api.pilots.localhost:8080/v1/machines/$OLD
```

## Follow-ups

- [x] #78 reap-and-react exit handling, e2e H9, gate section 21, @vivek7405, 2026-09-06

## What we would tell a customer

Your service was unreachable for two hours because the virtual machine behind
it exited and our host software did not notice. We have changed the host so
that an exit is detected the instant it happens, the machine is brought back on
its own disk within seconds, and a service replica is replaced automatically.
Nothing on your disk was lost.
