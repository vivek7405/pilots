/**
 * A WebSocket the test drives by hand.
 *
 * Injected through `ExecStreamOptions.WebSocket`, the same seam a caller uses
 * to drive the decoder from the `ws` package on a runtime with no global
 * WebSocket. It records the URL and the subprotocols it was dialled with,
 * which is how the auth assertion is made without a server.
 */

export class FakeWebSocket extends EventTarget {
  static last: FakeWebSocket | undefined

  readonly url: string
  readonly protocols: string[]
  readonly sent: Uint8Array[] = []
  binaryType = 'blob'
  readyState = 0
  closedWith: number | undefined

  constructor(url: string | URL, protocols?: string | string[]) {
    super()
    this.url = String(url)
    this.protocols = protocols === undefined ? [] : Array.isArray(protocols) ? protocols : [protocols]
    FakeWebSocket.last = this
  }

  send(data: Uint8Array): void {
    this.sent.push(new Uint8Array(data))
  }

  close(code?: number): void {
    this.readyState = 3
    this.closedWith = code
  }

  // --- what the test calls to act like a server -------------------------

  open(): void {
    this.readyState = 1
    this.dispatchEvent(new Event('open'))
  }

  /** Delivers one binary frame: an id byte followed by its payload. */
  frame(id: number, payload: Uint8Array | string = new Uint8Array()): void {
    const bytes = typeof payload === 'string' ? new TextEncoder().encode(payload) : payload
    const buf = new Uint8Array(bytes.length + 1)
    buf[0] = id
    buf.set(bytes, 1)
    this.dispatchEvent(new MessageEvent('message', { data: buf.buffer }))
  }

  /** Delivers a text frame, as hostd's proxy sends alongside the binary exit. */
  text(data: string): void {
    this.dispatchEvent(new MessageEvent('message', { data }))
  }

  /** Delivers an empty binary frame, which the decoder must ignore. */
  empty(): void {
    this.dispatchEvent(new MessageEvent('message', { data: new ArrayBuffer(0) }))
  }

  fail(): void {
    this.dispatchEvent(new Event('error'))
  }

  serverClose(): void {
    this.readyState = 3
    this.dispatchEvent(new CloseEvent('close'))
  }
}

/** Reads a readable stream to a string, for asserting on stdout/stderr. */
export async function collect(stream: NodeJS.ReadableStream): Promise<string> {
  const chunks: Buffer[] = []
  for await (const chunk of stream) chunks.push(chunk as Buffer)
  return Buffer.concat(chunks).toString('utf8')
}
