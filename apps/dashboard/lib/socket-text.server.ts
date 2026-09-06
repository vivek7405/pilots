/**
 * What a WebSocket frame actually is, on the way in.
 *
 * The framework hands a `WS` export the raw `ws` socket, and `ws` delivers a
 * message as a Buffer with the decoding left to the handler. A handler that
 * branches on `typeof data === 'string'` and otherwise treats the value as an
 * already-parsed object therefore gets an object with none of its own fields:
 * every message is silently dropped and the socket looks connected and dead.
 *
 * That is not hypothetical. The exec console's `parseArgv` had exactly that
 * shape, which is why its Run button did nothing in a browser while its unit
 * tests, which pass strings, stayed green.
 *
 * Server-only by extension, not by secrecy: it uses `Buffer`. It carries no
 * `'use server'`, so it is reachable from a `route.ts` and not from a
 * component.
 */

/** The decoded text of one inbound frame, or null when there is none. */
export function socketText(data: unknown): string | null {
  if (typeof data === 'string') return data;
  if (data instanceof ArrayBuffer) return Buffer.from(data).toString('utf8');
  if (ArrayBuffer.isView(data)) {
    return Buffer.from(data.buffer, data.byteOffset, data.byteLength).toString('utf8');
  }
  // `ws` can deliver a fragmented message as an array of Buffers.
  if (Array.isArray(data) && data.every((part) => ArrayBuffer.isView(part))) {
    return Buffer.concat(data.map((part) => Buffer.from(part.buffer, part.byteOffset, part.byteLength))).toString(
      'utf8',
    );
  }
  return null;
}

/** `socketText` plus a JSON parse. Returns null rather than throwing. */
export function socketJson<T>(data: unknown): T | null {
  const text = socketText(data);
  if (text === null) return null;
  try {
    return JSON.parse(text) as T;
  } catch {
    return null;
  }
}
