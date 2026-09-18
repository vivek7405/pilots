/**
 * What an epoch stamp from the fleet actually is.
 *
 * hostd stamps every timestamp it returns with `time.Now().Unix()` -- SECONDS
 * since the epoch, not milliseconds (`internal/machines/manager.go`,
 * `internal/machines/coldboot.go`, `internal/services/manager.go`). JavaScript
 * dates are milliseconds, so a raw `new Date(machine.last_activity)` lands in
 * January 1970 and every age on every page reads "56 years ago".
 *
 * The two are told apart by magnitude, which is unambiguous for any date this
 * product can be asked about: 1e11 seconds is the year 5138 and 1e11
 * milliseconds is March 1973, so anything smaller is seconds and anything
 * larger is milliseconds. A caller that already holds milliseconds (a JS
 * `Date.now()`, a `Date` turned into a number) is therefore left alone.
 */

/** Above this, a stamp is already milliseconds. */
const MS_FLOOR = 1e11;

/** One epoch stamp, in milliseconds, whichever unit it arrived in. */
export function epochMs(value: number): number {
  return Math.abs(value) < MS_FLOOR ? value * 1000 : value;
}
