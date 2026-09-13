/**
 * Browser-safe builder shapes.
 *
 * A builder is an ordinary machine with an extraordinary job, and
 * `GET /v1/builders` says so on the wire: it answers
 * `{"builders":[...]}` where each entry is the SAME object
 * `GET /v1/machines` returns (`apps/hostd/internal/api/builders.go`
 * `handleListBuilders` calls the shared `toAPI`). So this reuses the machine
 * shape rather than declaring a parallel one that could drift from it -- in
 * particular there is no `last_used_at` field anywhere in that struct, and a
 * type that invented one would render "Never" forever with nothing failing.
 */

import type { Machine } from '#modules/machines/types.ts';

export type Builder = Machine;

/** `GET /v1/builders`. */
export interface BuilderList {
  builders?: Builder[];
}

/** `POST /v1/builders/{host}/reset`, which answers 202. */
export interface BuilderReset {
  ok: boolean;
  /** The team's new build-cache epoch. Every host re-pulls past it. */
  epoch: number;
  /**
   * How many builder machines were destroyed on that host, normally 0 or 1.
   *
   * Zero is a success, not a miss: the epoch moves even when there is nothing
   * on that host to destroy, because "this builder is wedged" and "my layers
   * are wrong" are different complaints and the second one should not require
   * knowing which host holds a machine.
   */
  destroyed: number;
}
