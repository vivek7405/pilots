/**
 * STDOUT IS THE PROTOCOL CHANNEL.
 *
 * Over stdio transport, the MCP client parses every byte this process writes to
 * stdout as JSON-RPC. One `console.log` anywhere -- in a handler, in a
 * dependency, in a deprecation notice Node prints on a timer -- corrupts the
 * frame stream and the client's next read fails with a parse error that names
 * nothing useful.
 *
 * So the three console methods that write to stdout are pointed at stderr
 * before anything else runs. This module exists SEPARATELY from `server.ts`
 * for one reason: ES module imports are evaluated before the importing
 * module's body, so a reassignment at the top of `server.ts` would still run
 * after the MCP SDK had been loaded. Being the first import is the only way to
 * be first.
 */

const toStderr = (...args: unknown[]): void => {
  console.error(...args)
}

console.log = toStderr
console.info = toStderr
console.debug = toStderr

// `process.stdout.write` is deliberately NOT wrapped: it is what the transport
// itself writes frames with.
export const stdoutIsTheProtocol = true
