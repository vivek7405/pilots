/**
 * A module that prints to stdout on a timer, loaded with `--import`.
 *
 * It stands in for the real thing this guards against: a dependency, or Node
 * itself, writing a deprecation notice to stdout while the MCP server is
 * running. The delay is what makes it a fair test: it fires AFTER the stdout
 * guard is installed, so a working guard sends it to stderr and the frame
 * stream stays parseable. Remove the guard and the MCP test fails.
 */

setTimeout(() => {
  console.log('a dependency printed this to stdout')
}, 400).unref()
