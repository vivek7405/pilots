/**
 * The Next.js deployment adapter.
 *
 * The context fixture below is the shape Next actually passes, transcribed
 * from `packages/next/src/build/adapter/build-complete.ts` in the Next.js
 * source: `SharedRouteFields` (id, filePath, pathname, sourcePage, runtime,
 * assets, assetsHashes), `AdapterOutputs` (pages, middleware, appPages,
 * pagesApi, appRoutes, prerenders, staticFiles) and the `routing` bag
 * (beforeMiddleware, middlewareMatchers, beforeFiles, afterFiles,
 * dynamicRoutes, onMatch, fallback). A test built on an invented shape would
 * prove the adapter is self-consistent and nothing about whether Next can
 * drive it, so the fixture is the contract here.
 */

import test from 'node:test'
import assert from 'node:assert/strict'
import { mkdtemp, readFile } from 'node:fs/promises'
import { existsSync } from 'node:fs'
import { createRequire } from 'node:module'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { pathToFileURL } from 'node:url'

import adapter, {
  buildManifest,
  __resetForTest,
  APP_PORT,
  MANIFEST_NAME,
  MANIFEST_VERSION,
} from '../src/next.ts'

const REPO = '/repo'

function ctx(over: Record<string, unknown> = {}): any {
  return {
    nextVersion: '15.5.0',
    buildId: 'BUILD_ID_1',
    projectDir: REPO,
    repoRoot: REPO,
    distDir: '/repo/.next',
    config: {},
    routing: {
      beforeMiddleware: [],
      middlewareMatchers: [],
      beforeFiles: [],
      afterFiles: [],
      dynamicRoutes: [],
      onMatch: [],
      fallback: [],
    },
    outputs: {
      pages: [],
      appPages: [],
      pagesApi: [],
      appRoutes: [],
      prerenders: [],
      staticFiles: [],
    },
    ...over,
  }
}

function output(over: Record<string, unknown> = {}) {
  return {
    id: 'id',
    filePath: '/repo/.next/server/app/page.js',
    pathname: '/',
    sourcePage: '/page',
    runtime: 'nodejs',
    assets: {},
    assetsHashes: {},
    ...over,
  }
}

/**
 * Next loads an adapter as
 * `interopDefault(await import(pathToFileURL(require.resolve(path)).href))`
 * (server/config.ts and build/adapter/build-complete.ts). The require.resolve
 * is the trap: this package is ESM, and a subpath exported with only an
 * "import" condition -- which is how every other subpath here is exported --
 * makes a CJS resolve fail with ERR_PACKAGE_PATH_NOT_EXPORTED. `adapterPath:
 * '@pilots/sdk/next'` would then fail at config load with an error naming us,
 * and every test that imported ../src/next.ts directly would still pass.
 *
 * So this drives the real resolver against the real exports map. It needs the
 * built dist/, which is what the package's own build produces; with no dist it
 * skips rather than failing, because a source checkout is not a broken export
 * map.
 */
test("Next's own loader can resolve the adapter through the exports map", async (t) => {
  const require = createRequire(join(import.meta.dirname, '..', 'package.json'))
  let resolved: string
  try {
    resolved = require.resolve('@pilots/sdk/next')
  } catch (err) {
    const code = (err as NodeJS.ErrnoException).code
    if (code === 'ERR_PACKAGE_PATH_NOT_EXPORTED') {
      assert.fail(
        '@pilots/sdk/next is not resolvable by require.resolve, which is how Next loads an adapter. ' +
          'The subpath needs a "default" (or "require") condition, not "import" alone.',
      )
    }
    return t.skip(`@pilots/sdk is not linked here: ${code}`)
  }
  if (!existsSync(resolved)) return t.skip('dist/ is not built')
  const mod = await import(pathToFileURL(resolved).href)
  const loaded = mod.default ?? mod
  assert.equal(loaded.name, 'pilots')
  assert.equal(typeof loaded.onBuildComplete, 'function')
})

test('the adapter exports the two hooks Next calls, on the default export', () => {
  assert.equal(adapter.name, 'pilots')
  assert.equal(typeof adapter.modifyConfig, 'function')
  assert.equal(typeof adapter.onBuildComplete, 'function')
})

test("modifyConfig turns on standalone for a production build", () => {
  __resetForTest()
  const got = adapter.modifyConfig({}, {
    phase: 'phase-production-build',
    nextVersion: '15.5.0',
    projectDir: REPO,
  })
  assert.equal(got.output, 'standalone')
})

test('modifyConfig leaves a deliberate output mode alone', () => {
  __resetForTest()
  // 'export' is a static site. Forcing standalone over it would break the
  // build for no gain, which is why the adapter only fills an empty slot.
  for (const mode of ['export', 'standalone']) {
    const got = adapter.modifyConfig({ output: mode }, {
      phase: 'phase-production-build',
      nextVersion: '15.5.0',
      projectDir: REPO,
    })
    assert.equal(got.output, mode)
  }
})

test('modifyConfig touches nothing outside a production build', () => {
  __resetForTest()
  const config = { output: undefined, reactStrictMode: true }
  const got = adapter.modifyConfig(config, {
    phase: 'phase-development-server',
    nextVersion: '15.5.0',
    projectDir: REPO,
  })
  assert.equal(got.output, undefined)
  assert.equal(got, config, 'a non-build phase must return the config untouched')
})

test('the manifest carries the paths a router can serve without waking the machine', () => {
  __resetForTest()
  const m = buildManifest(
    ctx({
      config: { output: 'standalone' },
      outputs: {
        pages: [],
        appPages: [output()],
        pagesApi: [],
        appRoutes: [],
        prerenders: [output({ pathname: '/about', filePath: '/repo/.next/server/app/about.html' })],
        staticFiles: [output({ pathname: '/favicon.ico', filePath: '/repo/public/favicon.ico' })],
      },
    }),
  )
  assert.equal(m.version, MANIFEST_VERSION)
  assert.equal(m.adapter, 'pilots')
  assert.equal(m.port, APP_PORT)
  assert.equal(m.buildId, 'BUILD_ID_1')
  // Repository-relative and posix, because the manifest is read on a Linux
  // host no matter where the build ran.
  assert.deepEqual(m.static, [{ pathname: '/favicon.ico', filePath: 'public/favicon.ico' }])
  assert.deepEqual(m.prerendered, [{ pathname: '/about', filePath: '.next/server/app/about.html' }])
  assert.equal(m.distDir, '.next')
  assert.equal(m.standalone, true)
  assert.deepEqual(m.warnings, [], 'a standalone build with no edge routes warns about nothing')
})

test('redirects and rewrites are told apart by status', () => {
  __resetForTest()
  const m = buildManifest(
    ctx({
      config: { output: 'standalone' },
      routing: {
        beforeFiles: [
          { source: '/old', sourceRegex: '^/old$', destination: '/new', status: 308 },
          { source: '/api/:p*', sourceRegex: '^/api/(.*)$', destination: 'https://up.example/:p*' },
        ],
        afterFiles: [{ source: '/fallback', sourceRegex: '^/fallback$', destination: '/index' }],
        onMatch: [{ sourceRegex: '^/(.*)$', headers: { 'x-frame-options': 'DENY' } }],
        dynamicRoutes: [{ source: '/p/:id', sourceRegex: '^/p/([^/]+)$' }],
      },
    }),
  )
  assert.equal(m.routes.redirects.length, 1)
  assert.equal(m.routes.redirects[0].status, 308)
  assert.equal(m.routes.redirects[0].destination, '/new')
  assert.equal(m.routes.rewrites.length, 2, 'a non-3xx beforeFiles rule and every afterFiles rule')
  assert.equal(m.routes.headers[0].headers?.['x-frame-options'], 'DENY')
  assert.equal(m.routes.dynamic.length, 1)
})

test('a rule carrying a predicate is flagged rather than flattened', () => {
  __resetForTest()
  // has/missing are Next's own predicate dialect. A router that does not
  // understand them must skip the rule, not apply it unconditionally -- an
  // unconditionally-applied conditional redirect is a redirect loop.
  const m = buildManifest(
    ctx({
      config: { output: 'standalone' },
      routing: {
        beforeFiles: [
          {
            source: '/x',
            sourceRegex: '^/x$',
            destination: '/y',
            status: 307,
            has: [{ type: 'header', key: 'x-beta' }],
          },
          { source: '/a', sourceRegex: '^/a$', destination: '/b', status: 307 },
        ],
      },
    }),
  )
  assert.equal(m.routes.redirects[0].conditional, true)
  assert.equal(m.routes.redirects[1].conditional, false)
})

test('an edge-runtime route is a warning, because pilots has one tier', () => {
  __resetForTest()
  const m = buildManifest(
    ctx({
      config: { output: 'standalone' },
      outputs: {
        appPages: [output()],
        appRoutes: [output({ pathname: '/api/edge', runtime: 'edge' })],
        middleware: output({ pathname: '/_middleware', runtime: 'edge' }),
        pages: [],
        pagesApi: [],
        prerenders: [],
        staticFiles: [],
      },
    }),
  )
  assert.equal(m.counts.edgeRuntime, 2)
  assert.equal(m.counts.middleware, 1)
  assert.equal(m.warnings.length, 1)
  assert.match(m.warnings[0], /edge runtime/)
  assert.match(m.warnings[0], /\/api\/edge/)
})

test('a non-standalone build says so, since that is the big image', () => {
  __resetForTest()
  const m = buildManifest(ctx({ config: {} }))
  assert.equal(m.standalone, false)
  assert.equal(m.warnings.length, 1)
  assert.match(m.warnings[0], /standalone/)
})

test('modifyConfig and onBuildComplete agree about standalone across the two calls', async () => {
  __resetForTest()
  // The two hooks are separate calls with no shared context. A Next version
  // that does not echo the modified output mode back in onBuildComplete's
  // `config` must still produce a manifest that says standalone=true, or the
  // manifest would warn about a problem the adapter itself just fixed.
  adapter.modifyConfig({}, { phase: 'phase-production-build', nextVersion: '15.5.0', projectDir: REPO })
  const m = buildManifest(ctx({ config: {} }))
  assert.equal(m.standalone, true)
  assert.deepEqual(m.warnings, [])
})

test('onBuildComplete writes exactly one file, and it is valid JSON', async () => {
  __resetForTest()
  const dist = await mkdtemp(join(tmpdir(), 'pilots-next-'))
  await adapter.onBuildComplete(
    ctx({
      distDir: dist,
      repoRoot: dist,
      config: { output: 'standalone' },
      outputs: {
        appPages: [output()],
        pages: [],
        pagesApi: [],
        appRoutes: [],
        prerenders: [],
        staticFiles: [],
      },
    }),
  )
  const raw = await readFile(join(dist, MANIFEST_NAME), 'utf8')
  const parsed = JSON.parse(raw)
  assert.equal(parsed.adapter, 'pilots')
  assert.equal(parsed.counts.appPages, 1)
  assert.ok(raw.endsWith('\n'), 'a file with no trailing newline is a diff that never settles')
})

test('a context missing every optional field still produces a manifest', () => {
  __resetForTest()
  // Next may omit what a given build has none of, and an adapter that threw
  // on a missing array would fail the build of the simplest possible app.
  const m = buildManifest({
    nextVersion: '15.5.0',
    buildId: 'b',
    projectDir: REPO,
    repoRoot: REPO,
    distDir: '/repo/.next',
    config: { output: 'standalone' },
    routing: {},
    outputs: {},
  } as any)
  assert.equal(m.counts.pages, 0)
  assert.deepEqual(m.static, [])
  assert.deepEqual(m.routes.rewrites, [])
})
