# AGENTS.md — packages/cli

The `pilot` CLI and the MCP server, one binary. TypeScript run by Node's type
stripping: erasable syntax only, `.ts` import extensions, no build step, Node 24.

## Where the wire types live

`apps/hostd/internal/api/types.go` and `apps/hostd/internal/compose/` are the
contract. `sdks/js/src/types.ts` mirrors them, and `sdks/js/test/drift.test.ts`
fails when either side carries a field the other lacks.

**This package never declares a wire shape of its own.** A local interface
describing a response is a third copy that nothing checks, and it will be the
one that is wrong.

## The stdout rule

stdout is the API's response under `--json`, and the MCP protocol under
`pilot mcp`. Every diagnostic goes to stderr. `src/mcp/quiet.ts` enforces it
for the MCP server and is imported before anything else in `src/mcp/server.ts`
for that reason: one stray `console.log` from a dependency corrupts the
JSON-RPC framing and the failure reads as a broken client.

## The error contract

Every non-2xx body is `{error, code, next, details}`. The codes are a closed
list in `apps/hostd/internal/api/errors.go`. The CLI prints `error: <error>`
and then `→ <next>` on its own line; the MCP passes the server's body through
verbatim, because an agent branching on `code` needs the body, not a summary.

## The one planner

Compose parsing and framework detection both run on the host, at
`POST /v1/compose/plan` and `POST /v1/plan`. `src/compose/run.ts` executes
plans and knows nothing about compose or about frameworks. Do not add a parser
or a detector here: there used to be one, it was invisible to the dashboard and
the push path, and moving it is what #77 was.

## The skill

`skill/pilots/` is the source and is what ships; there is no build step and no
second copy. `src/mcp/skill.ts` resolves an app's own `.agents/skills/pilots/`
first, then the packaged one.

There used to be a `prepack` hook copying it to `resources/`, with both in
`files`. It shipped the corpus twice and, under a publish with
`--ignore-scripts`, shipped none of it.

The tool list lives in five places: the registrations in `src/mcp/tools.ts`,
`TOOLS` in `test/mcp.test.ts`, `MCP_TOOLS` in `scripts/e2e.mjs`, the README and
`ARCHITECTURE.md`. A test holds the README to the registrations; keep the other
three in step by hand when you add a tool.

## Tests

`npm run test:cli` from the repository root. It builds the SDK first, which
`npm test --workspace=packages/cli` does not, so running the workspace script
alone fails at `tsc` whenever `sdks/js/dist` is stale.

The fake hostd is `test/helpers/fake-api.ts`. No test here reaches a real
fleet: what a command SENDS is the thing under test, and the fake records
exactly that.
