# AGENTS.md — sdks/go

`github.com/vivek7405/pilots/sdks/go`, a module of its own inside the
repository. Run every Go command from this directory.

## Wire types

`types.go` mirrors `apps/hostd/internal/api` and
`apps/hostd/internal/compose`; `wireTypes` at the bottom registers every
mirrored struct. `types_drift_test.go` parses hostd's Go source with `go/ast`
and fails on a missing or an extra tag, in either direction.

Shapes from the compose package carry a `Compose` prefix, because `Plan`,
`Step` and `Request` are far too generic to export unqualified.

A struct listed in hostd and absent from `wireTypes` fails the drift test. That
is deliberate: a wire shape nobody mirrored is one no Go caller can read.

## Errors

`errors.go`. Every non-2xx becomes `*Error`, carrying `Code`, `Next` and the
undecoded `Details`, so a code this version has never heard of still reaches
the caller.

    errors.Is(err, pilots.ErrNotFound)

    var gate *pilots.HealthGateFailed
    errors.As(err, &gate)

The typed cases are `*QuotaExceeded`, `*ComposePlanError`, `*BuildFailed`,
`*HealthGateFailed` and `*UnknownFramework`. The last two are matched on the
body's `code` and never on the status alone.

## Checks

From this directory: `gofmt -l .` (it must print nothing), `go vet ./...`,
`go test ./...`.
