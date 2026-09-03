/**
 * `secret://` references, resolved on the client.
 *
 * hostd never sees the reference as a value. It returns `secret_refs` -- a map
 * from an environment key to a secret NAME -- and the CLI turns those names
 * into values here and sends them as `secret_env`, which hostd seals. That
 * split is the point: a compose file checked into a repo carries names, and
 * the values live in the operator's credentials file or their environment.
 */

import type { Credentials } from '../config.ts'
import { CliError } from '../output.ts'

export interface ResolveOptions {
  app: string
  env?: NodeJS.ProcessEnv
  credentials?: Credentials | null
}

/** `database_url` becomes `PILOT_SECRET_DATABASE_URL`. */
export function envVarFor(name: string): string {
  return `PILOT_SECRET_${name.toUpperCase().replace(/[^A-Z0-9]/g, '_')}`
}

/**
 * Turns `{ENV_KEY: secret_name}` into `{ENV_KEY: value}`.
 *
 * The environment wins over the file, so a CI job overrides a developer's
 * local value without editing anything. Every missing name is reported at
 * once: resolving one at a time would mean one failed deploy per secret.
 */
export function resolveSecrets(
  refs: Record<string, string> | undefined,
  opts: ResolveOptions,
): Record<string, string> {
  const env = opts.env ?? process.env
  const stored = opts.credentials?.secrets?.[opts.app] ?? {}
  const out: Record<string, string> = {}
  const missing: string[] = []

  for (const [key, name] of Object.entries(refs ?? {})) {
    const fromEnv = env[envVarFor(name)]
    const value = fromEnv ?? stored[name]
    if (value === undefined) {
      missing.push(name)
      continue
    }
    out[key] = value
  }

  if (missing.length > 0) {
    const unique = [...new Set(missing)].sort()
    throw new CliError(
      `no value for ${unique.length === 1 ? 'secret' : 'secrets'} ${unique.join(', ')}: ` +
        `set ${unique.map(envVarFor).join(', ')} or run \`pilot add postgres\` on this machine`,
    )
  }
  return out
}

/** Every secret name a plan needs, for one up-front resolution pass. */
export function collectRefs(steps: { secret_refs?: Record<string, string> }[]): string[] {
  const names = new Set<string>()
  for (const step of steps) {
    for (const name of Object.values(step.secret_refs ?? {})) names.add(name)
  }
  return [...names].sort()
}
