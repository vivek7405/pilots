/**
 * Which rows are builders.
 *
 * A builder is the machine hostd creates for itself to build a team's images
 * on. It is a real row with a real id, it counts against nothing a person
 * chose, and it has no URL, so it does not belong in a list of the things
 * someone deployed -- but hiding it entirely leaves an unexplained gap when a
 * build is slow and a reader is looking for the reason.
 *
 * The signal is the NAME PREFIX, which is what the engine itself reads: the
 * quota loop, the idle monitor, the router and the create path all test the
 * same prefix (`apps/hostd/internal/quota/quota.go`, `BuilderNamePrefix`).
 * There is no `kind` field on the wire to read instead, so this mirrors the
 * one signal that exists rather than inventing a second one that could
 * disagree with it.
 *
 * Browser-safe: the live list filters on it after every socket delta.
 */

import type { Machine } from '#modules/machines/types.ts';

/** `apps/hostd/internal/quota/quota.go` `BuilderNamePrefix`. */
export const BUILDER_NAME_PREFIX = 'builder-';

export function isBuilderName(name: string | undefined): boolean {
  return typeof name === 'string' && name.startsWith(BUILDER_NAME_PREFIX);
}

export function isBuilder(machine: Pick<Machine, 'name'>): boolean {
  return isBuilderName(machine.name);
}
