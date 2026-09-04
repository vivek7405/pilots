/**
 * Requests carry a knobs PATCH, never a full policy.
 *
 * hostd merges a request's knobs onto a value it has already seeded, so a key
 * the caller left out has to be absent from the JSON rather than present as a
 * zero. Typing a request field as the full `Knobs` forces the caller to spell
 * all four, and the three they did not care about are merged as zeros --
 * `auto_start: false` suspends the replica after a minute and the router then
 * refuses to wake it, so a call that only raised a concurrency limit leaves a
 * permanently dead URL behind.
 *
 * `tsc` never sees this file (`tsconfig.json` includes `src` only), so the
 * guard reads the source rather than relying on a type error.
 */

import test from 'node:test'
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { join } from 'node:path'

const typesTS = join(import.meta.dirname, '..', 'src', 'types.ts')
const src = readFileSync(typesTS, 'utf8')

/** The declared type of one property of one interface. */
function propertyType(iface: string, prop: string): string | undefined {
  const body = new RegExp(`^export interface ${iface} \\{$([\\s\\S]*?)^\\}$`, 'm').exec(src)
  if (!body) return undefined
  const match = new RegExp(`^\\s+${prop}\\??:\\s*(.+?)\\s*$`, 'm').exec(body[1]!)
  return match?.[1]
}

test('the parser found the interfaces it is checking', () => {
  assert.equal(propertyType('Machine', 'knobs'), 'Knobs')
})

test('every request that carries knobs carries the patch', () => {
  for (const iface of [
    'CreateMachineRequest',
    'CreateServiceRequest',
    'DeployRequest',
    'ComposeStep',
  ]) {
    assert.equal(
      propertyType(iface, 'knobs'),
      'KnobsPatch',
      `${iface}.knobs must be a patch: hostd merges it, so an unmentioned field ` +
        'has to stay absent rather than arrive as a zero',
    )
  }
})

test('a response carries the full policy', () => {
  // The other direction is just as load-bearing. A response always spells all
  // four, so reading `machine.knobs.auto_start` must not be an optional-chain.
  for (const iface of ['Machine', 'Service']) {
    assert.equal(propertyType(iface, 'knobs'), 'Knobs')
  }
})

test('the patch is derived from Knobs rather than copied', () => {
  // Copying the four keys would let a knob added to `Knobs` (which the drift
  // test holds to hostd) be forgotten here, leaving a field no caller could
  // set without zeroing its neighbours.
  assert.match(src, /^export type KnobsPatch = Partial<Knobs>$/m)
})
