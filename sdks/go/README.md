# `github.com/vivek7405/pilots/sdks/go`

The typed Go client for the pilots API: instant sandboxes and durable
production services on one primitive, Firecracker microVMs.

One dependency, `github.com/coder/websocket`, which is the same one hostd
speaks the exec stream with.

```
go get github.com/vivek7405/pilots/sdks/go
```

## Construction

```go
c := pilots.New(os.Getenv("PILOT_API_KEY"))

m, err := c.Machines.Create(ctx, pilots.CreateMachineRequest{Name: "demo"})
if err != nil {
	log.Fatal(err)
}
fmt.Println(m.URL) // https://demo.pilotrun.app
```

The base URL comes from `WithBaseURL`, then `PILOT_API_URL`, then
`https://api.pilotrun.app`. Every host serves the identical API, so any host in
the fleet is a valid endpoint: there is no control-plane tier to be down, and a
write that arrives at the wrong host is forwarded by hostd itself.

`WithHTTPClient` replaces the `*http.Client`, which is where retries, pooling
or tracing belong. Nothing inside this package does any of them.

## Methods

One method per route, grouped by the noun it acts on.

| Call | Route |
| --- | --- |
| `c.Health(ctx)` | `GET /v1/health` |
| `c.Machines.Create/List/Get/Destroy` | `/v1/machines` |
| `c.Machines.Exec(ctx, id, req)` | `POST /v1/machines/{id}/exec` |
| `c.Machines.ExecStream(ctx, id, argv, opts)` | `GET /v1/machines/{id}/exec/stream` |
| `c.Machines.Logs / FollowLogs` | `GET /v1/machines/{id}/logs` |
| `c.Machines.Suspend/Wake/Stop/Start` | `POST /v1/machines/{id}/…` |
| `c.Machines.Checkpoint / ListCheckpoints` | `/v1/machines/{id}/checkpoints` |
| `c.Machines.Promote / Volume` | `/v1/machines/{id}/…` |
| `c.Checkpoints.Restore / Get` | `/v1/checkpoints/{id}` |
| `c.Builds.Create / Logs` | `/v1/builds` |
| `c.Services.Create/List/Get/Patch/Deploy/Rollback/Releases` | `/v1/services` |
| `c.Domains.Add / List / Remove` | `/v1/domains` |
| `c.Volumes.Create / List` | `/v1/volumes` |
| `c.Hosts.List(ctx)` | `GET /v1/hosts` |
| `c.APIKeys.Create / Revoke / List` | `/v1/api-keys` |
| `c.Quotas.Get / Put` | `/v1/quotas/{org}` |
| `c.Usage.Get(ctx, since, until)` | `GET /v1/usage` |
| `c.Compose.Plan(ctx, req)` | `POST /v1/compose/plan` |

Every wire struct carries the name and the JSON tags of hostd's own, with the
structs from hostd's compose package under a `Compose` prefix.
`types_drift_test.go` parses hostd's source on every `go test` and fails naming
the struct and the tag when the two sides drift, in either direction.

`Health` carries `StoreVersion`, the sum of that host's replica version
vector: how many changes, from every host, it has applied. Comparable across
hosts, so two hosts far apart on it are a replication problem. 0 on a
single-box SQLite host, which has no replica.

## Errors

```go
_, err := c.Machines.Create(ctx, req)

if errors.Is(err, pilots.ErrNotFound) { … }

var quota *pilots.QuotaExceeded
if errors.As(err, &quota) {
	log.Printf("%s quota: %d of %d used", quota.Quota, quota.Used, quota.Limit)
}

var failed *pilots.BuildFailed
if errors.As(err, &failed) {
	for _, line := range failed.Lines {
		log.Println(line.Step, line.Line)
	}
}
```

| Type | When | Carries |
| --- | --- | --- |
| `*Error` | any non-2xx | `StatusCode`, `Body`, `Message` |
| `ErrNotFound` | 404 | wrapped by `*Error`, so `errors.Is` matches |
| `*QuotaExceeded` | 429 | `Quota`, `Limit`, `Used`, `Scope` |
| `*ComposePlanError` | a compose plan hostd will not accept | `Unsupported` |
| `*BuildFailed` | a build that failed | `ID`, `Reason`, `Lines` |

`Scope` is `"host"` when the ceiling is the host's rather than the org's, which
is how builds are limited.

## Streaming exec

```go
s, err := c.Machines.ExecStream(ctx, m.ID,
	[]string{"bash", "-c", "npm run build"},
	pilots.ExecStreamOptions{Dir: "/home/sprite/app", Env: map[string]string{"CI": "1"}})
if err != nil {
	log.Fatal(err)
}
stdout, stderr, code, err := s.Output()
```

**Read both streams, or call `Output`.** `Stdout` and `Stderr` are `io.Pipe`
readers, so the frame loop blocks until a consumer reads. That is deliberate:
it gives a slow consumer back-pressure instead of unbounded growth. It is also
exactly the caveat `os/exec` makes about `StdoutPipe`, and it bites the same
way: reading one to completion while never reading the other deadlocks.
`Output` drains both concurrently and then waits.

**`Stdin` is nil unless you ask for it.** Set `ExecStreamOptions{Stdin: true}`
and `s.Stdin` becomes an `io.WriteCloser`; each `Write` is one stdin frame and
`Close` is the stdin EOF frame. It is off by default because a process holding
an open stdin it never reads hangs.

**A text verdict LEADS the binary exit frame.** hostd sends
`{"type":"exit","exit_code":n}` and then frame `3`, in that order, because the
binary frame carries the code in one byte: a command killed by a signal has an
exit code of -1, which one byte reports as 255 and no reader can tell from a
command that genuinely exited 255. `Wait` settles on whichever verdict arrives
first and closes the socket, so the text one has to be first for the
untruncated code to be the one you get. Frame `3` still follows it, unchanged,
for a client that reads only binary frames.

**An exec that names no user runs as `sprite`.** The guest image bakes that
account at uid 1000 with home `/home/sprite` and Node 24 on `PATH`, so a
command needs neither a `User` nor a `Dir` to land where these examples assume.

**A close with no exit frame is an error.** `Wait` returns it rather than a
code of 0: an exit frame means the output that preceded it has already
arrived, while a dropped socket means nobody knows what the command did.

The key travels in the `Authorization` header, since Go can set handshake
headers. hostd also accepts it as the `authorization.bearer.<key>` subprotocol,
which is how the browser-facing JS client dials.

## Builds

```go
build, err := c.Builds.Create(ctx, contextTar)
for line, err := range build.Lines {
	if err != nil {
		log.Fatal(err)
	}
	log.Println(line.Step, line.Line)
}
```

Or, when only the outcome matters:

```go
rootfsBuildID, err := build.Result()
```

hostd answers 200 before the build starts, so the log is watchable while it
runs. The status code therefore cannot be the verdict: the last line is.
`Result` reads it, and returns a `*BuildFailed` both when that line carries an
error and when the stream ended with no verdict at all.
