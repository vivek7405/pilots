/**
 * The pilots deployment adapter for Next.js.
 *
 * Next.js defines an adapter interface -- `NextAdapter`, registered through
 * `adapterPath` in next.config.js or the NEXT_ADAPTER_PATH environment
 * variable -- that lets a host take part in the build rather than guess at its
 * output afterwards. This is pilots implementing that interface, the same
 * relationship the TanStack AI provider in ./tanstack.ts has with TanStack:
 * their contract, our implementation, so pilots is a first-class target
 * instead of a platform that happens to run `next start`.
 *
 * It does two things, and deliberately not more.
 *
 * `modifyConfig` turns on `output: 'standalone'` for a production build. That
 * is the single highest-value thing a host can do to a Next build and the one
 * most often missed: standalone makes Next trace the files each entrypoint
 * actually needs and emit a self-contained server, instead of the image
 * carrying the whole repository and a full node_modules. It is not forced --
 * a project that has already chosen an output mode keeps it, because
 * overriding a deliberate choice is worse than a larger image.
 *
 * `onBuildComplete` writes ONE file, `<distDir>/pilots-deploy.json`, holding
 * what pilots can act on: the static and prerendered paths the router can
 * serve without waking the machine, the redirect/rewrite/header rules it can
 * answer at the edge, and the counts and warnings a person needs to read.
 * One file because a second copy of a contract is a thing to keep in sync,
 * and the traced asset lists are Next's to own, not ours to duplicate.
 *
 * This module is dependency-free on purpose: it runs inside a build, where
 * the pilots API is not reachable and a client would be dead weight.
 *
 * Usage, in next.config.js:
 *
 *     module.exports = { adapterPath: '@pilots/sdk/next' }
 *
 * or, with no config change at all:
 *
 *     NEXT_ADAPTER_PATH=@pilots/sdk/next next build
 */

import { mkdir, writeFile } from 'node:fs/promises'
import { dirname, join, relative, sep } from 'node:path'

/** The port every pilots recipe declares and the router dials. */
export const APP_PORT = 8080

/** The manifest's schema version. Bumped when a consumer must be changed. */
export const MANIFEST_VERSION = 1

/** The file the adapter writes, inside the build's distDir. */
export const MANIFEST_NAME = 'pilots-deploy.json'

/**
 * A routing rule, flattened from Next's shape into the subset a router can
 * act on. Next's own `Route` carries more (`has`/`missing` predicates, and a
 * `sourceRegex` whose dialect is Next's); those travel verbatim so a consumer
 * that understands them can use them, and one that does not can skip the rule
 * rather than mis-apply it.
 */
export interface ManifestRoute {
  source?: string
  sourceRegex: string
  destination?: string
  status?: number
  headers?: Record<string, string>
  /** True when the rule carries a predicate this manifest does not flatten. */
  conditional: boolean
}

/** A path the router can answer from the release without waking the machine. */
export interface ManifestStatic {
  pathname: string
  /** Where the bytes are, relative to the repository root. */
  filePath: string
}

export interface PilotsNextManifest {
  version: number
  adapter: 'pilots'
  nextVersion: string
  buildId: string
  /** distDir relative to the repository root, so a consumer can find it. */
  distDir: string
  /** True when modifyConfig turned standalone on, or it was already on. */
  standalone: boolean
  port: number
  static: ManifestStatic[]
  prerendered: ManifestStatic[]
  routes: {
    redirects: ManifestRoute[]
    rewrites: ManifestRoute[]
    headers: ManifestRoute[]
    dynamic: ManifestRoute[]
  }
  counts: {
    pages: number
    appPages: number
    appRoutes: number
    pagesApi: number
    prerenders: number
    staticFiles: number
    middleware: number
    edgeRuntime: number
  }
  /** Things a person should read before trusting the deploy. */
  warnings: string[]
}

// The shapes below mirror the parts of Next's own types this adapter reads.
// They are declared structurally rather than imported from `next`, so the
// package has no dependency on Next and the adapter can be typechecked and
// tested without one. Everything is optional that Next may not emit.

interface NextRoute {
  source?: string
  sourceRegex?: string
  destination?: string
  status?: number
  headers?: Record<string, string>
  has?: unknown[]
  missing?: unknown[]
}

interface NextOutput {
  id?: string
  filePath?: string
  pathname?: string
  sourcePage?: string
  runtime?: 'nodejs' | 'edge'
  assets?: Record<string, string>
}

interface BuildCompleteContext {
  routing: {
    beforeFiles?: NextRoute[]
    afterFiles?: NextRoute[]
    dynamicRoutes?: NextRoute[]
    onMatch?: NextRoute[]
    fallback?: NextRoute[]
    beforeMiddleware?: NextRoute[]
    middlewareMatchers?: NextRoute[]
  }
  outputs: {
    pages?: NextOutput[]
    middleware?: NextOutput | undefined
    appPages?: NextOutput[]
    pagesApi?: NextOutput[]
    appRoutes?: NextOutput[]
    prerenders?: NextOutput[]
    staticFiles?: NextOutput[]
  }
  projectDir: string
  repoRoot: string
  distDir: string
  config: Record<string, unknown>
  nextVersion: string
  buildId: string
}

interface ModifyConfigContext {
  phase: string
  nextVersion: string
  projectDir: string
}

/** Next's production build phase, by its documented string value. */
const PRODUCTION_BUILD = 'phase-production-build'

/**
 * Whether the adapter turned standalone on for this process.
 *
 * modifyConfig and onBuildComplete are separate calls with no shared context,
 * and the manifest has to record which output mode the build actually used.
 * Reading it back off the config in onBuildComplete is the primary source;
 * this is the fallback for a Next version that does not echo it there.
 */
let forcedStandalone = false

function toManifestRoute(r: NextRoute): ManifestRoute {
  const out: ManifestRoute = {
    sourceRegex: r.sourceRegex ?? '',
    conditional: Boolean((r.has && r.has.length) || (r.missing && r.missing.length)),
  }
  if (r.source !== undefined) out.source = r.source
  if (r.destination !== undefined) out.destination = r.destination
  if (r.status !== undefined) out.status = r.status
  if (r.headers !== undefined) out.headers = r.headers
  return out
}

/**
 * A path relative to the repository root, in posix form.
 *
 * The manifest is read on a Linux host regardless of where the build ran, so
 * a Windows backslash in it would be a path that resolves to nothing. An
 * absolute path that escapes the repository root is left as Next gave it,
 * because silently rewriting it would hide a real problem.
 */
function repoRelative(repoRoot: string, filePath: string | undefined): string {
  if (!filePath) return ''
  const rel = relative(repoRoot, filePath)
  if (!rel || rel.startsWith('..')) return filePath.split(sep).join('/')
  return rel.split(sep).join('/')
}

function toStatic(repoRoot: string, o: NextOutput): ManifestStatic {
  return { pathname: o.pathname ?? '', filePath: repoRelative(repoRoot, o.filePath) }
}

/**
 * Build the manifest from a build-complete context.
 *
 * Exported separately from the adapter so it can be tested against a recorded
 * context without running a Next build: the adapter's value is entirely in
 * what this produces, and a test that only proved the object has two
 * functions would prove nothing.
 */
export function buildManifest(ctx: BuildCompleteContext): PilotsNextManifest {
  const { outputs, routing, repoRoot, config } = ctx
  const out = <T,>(v: T[] | undefined): T[] => v ?? []

  const everyOutput = [
    ...out(outputs.pages),
    ...out(outputs.appPages),
    ...out(outputs.appRoutes),
    ...out(outputs.pagesApi),
    ...(outputs.middleware ? [outputs.middleware] : []),
  ]
  const edge = everyOutput.filter((o) => o.runtime === 'edge')

  const warnings: string[] = []
  if (edge.length > 0) {
    // pilots has one primitive: a microVM running Node. There is no second,
    // edge tier for these to land on, and they run on Node instead. That is
    // usually fine and occasionally not -- edge middleware written against
    // the Web-only globals is fine, one reaching for a Node API is not --
    // so it is said once, at build time, rather than discovered at runtime.
    warnings.push(
      `${edge.length} output(s) are built for the edge runtime (${edge
        .map((o) => o.pathname ?? o.id ?? '?')
        .slice(0, 5)
        .join(', ')}). pilots runs every route on Node in one machine; there is no separate edge tier.`,
    )
  }
  const standalone = config.output === 'standalone' || forcedStandalone
  if (!standalone) {
    warnings.push(
      "output is not 'standalone', so the image ships the whole repository and a full node_modules. Set output: 'standalone' in next.config.js, or let this adapter set it by not choosing another output mode.",
    )
  }

  return {
    version: MANIFEST_VERSION,
    adapter: 'pilots',
    nextVersion: ctx.nextVersion,
    buildId: ctx.buildId,
    distDir: repoRelative(repoRoot, ctx.distDir),
    standalone,
    port: APP_PORT,
    static: out(outputs.staticFiles).map((o) => toStatic(repoRoot, o)),
    prerendered: out(outputs.prerenders).map((o) => toStatic(repoRoot, o)),
    routes: {
      // beforeFiles runs before the filesystem, which is what a redirect
      // and a proxying rewrite are; afterFiles runs once nothing matched.
      redirects: out(routing.beforeFiles)
        .filter((r) => typeof r.status === 'number' && r.status >= 300 && r.status < 400)
        .map(toManifestRoute),
      rewrites: out(routing.beforeFiles)
        .filter((r) => !(typeof r.status === 'number' && r.status >= 300 && r.status < 400))
        .concat(out(routing.afterFiles))
        .map(toManifestRoute),
      headers: out(routing.onMatch).map(toManifestRoute),
      dynamic: out(routing.dynamicRoutes).map(toManifestRoute),
    },
    counts: {
      pages: out(outputs.pages).length,
      appPages: out(outputs.appPages).length,
      appRoutes: out(outputs.appRoutes).length,
      pagesApi: out(outputs.pagesApi).length,
      prerenders: out(outputs.prerenders).length,
      staticFiles: out(outputs.staticFiles).length,
      middleware: outputs.middleware ? 1 : 0,
      edgeRuntime: edge.length,
    },
    warnings,
  }
}

/**
 * The adapter itself.
 *
 * Typed structurally rather than as Next's `NextAdapter` so this package does
 * not depend on Next; test/next.test.ts pins the shape against the interface
 * as Next declares it, which is the assertion that actually matters.
 */
export const adapter = {
  name: 'pilots',

  modifyConfig(config: Record<string, unknown>, ctx: ModifyConfigContext): Record<string, unknown> {
    if (ctx.phase !== PRODUCTION_BUILD) return config
    // A project that chose an output mode keeps it. 'standalone' is the good
    // default for a host, not a decision to take away from someone who has
    // already made a different one -- 'export' in particular means a static
    // site, and overriding it would break the build for a gain of nothing.
    if (config.output !== undefined) return config
    forcedStandalone = true
    return { ...config, output: 'standalone' }
  },

  async onBuildComplete(ctx: BuildCompleteContext): Promise<void> {
    const manifest = buildManifest(ctx)
    const path = join(ctx.distDir, MANIFEST_NAME)
    await mkdir(dirname(path), { recursive: true })
    await writeFile(path, JSON.stringify(manifest, null, 2) + '\n', 'utf8')
    for (const w of manifest.warnings) console.warn(`pilots: ${w}`)
    console.log(
      `pilots: wrote ${MANIFEST_NAME} (${manifest.counts.staticFiles} static, ` +
        `${manifest.counts.prerenders} prerendered, standalone=${manifest.standalone})`,
    )
  },
}

/** Next calls interopDefault on the loaded module, so default is the entry. */
export default adapter

/** Test seam: reset the module-level standalone flag between cases. */
export function __resetForTest(): void {
  forcedStandalone = false
}
