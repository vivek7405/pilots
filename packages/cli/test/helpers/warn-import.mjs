/**
 * Emits two warnings on a timer, loaded with `--import`.
 *
 * The timer is what makes this a fair test of the filter in `bin/pilot.js`:
 * both warnings fire after the shim has replaced Node's warning listener, which
 * is when the real type-stripping warning fires too. One must be swallowed and
 * the other must not.
 */

setTimeout(() => {
  const stripping = new Error('Type Stripping is an experimental feature and might change at any time')
  stripping.name = 'ExperimentalWarning'
  process.emitWarning(stripping)
  process.emitWarning('something a user should actually see', 'DeprecationWarning')
}, 100).unref()
