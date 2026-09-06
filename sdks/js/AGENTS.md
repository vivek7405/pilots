# AGENTS.md — sdks/js

`@pilots/sdk`, the typed client. Built by `tsc` into `dist/`. The exported
names are listed explicitly in `src/index.ts` rather than star-exported,
because `ComposePlanError` is both a wire shape and an error class and only one
of them should be reachable under that name.

## Wire types

`src/types.ts` mirrors `apps/hostd/internal/api` and
`apps/hostd/internal/compose`. `test/drift.test.ts` parses hostd's Go source on
every run and fails naming the struct and the tag when the two disagree.

A new struct in hostd means a new interface here **in the same pull request**.
The drift test is what makes that non-optional, and it is the reason a field
added in Go cannot quietly never reach a consumer.

## Errors

`src/errors.ts`, one class per case a caller branches on: `NotFoundError`,
`QuotaExceededError`, `ComposePlanError`, `BuildFailedError`,
`HealthGateError`, `UnknownFrameworkError`.

Matched in `src/http.ts` by the body's `code`, never by status alone. 422 is
the shape of the health gate's answer today, and a later 422 for something else
must not arrive typed as this one.

Every error carries `code`, `next`, `details` and `body`, on the base class, so
a code this version has never heard of still reaches the caller with its next
step attached rather than being dropped on the way through.

## Streams

`BuildStream` and `ExecStream`. A build's verdict is its LAST NDJSON line: the
HTTP status is 200 before the outcome is known, because the whole point is that
a ten-minute build is watchable while it runs.

## Tests

`npm test --workspace=sdks/js`.
