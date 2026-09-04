/**
 * The exec console, in a real DOM, fed frames that carry raw bytes.
 *
 * The route base64-encodes each stdout / stderr chunk as it arrives, so a
 * frame boundary can fall INSIDE a multi-byte UTF-8 sequence. The console has
 * to decode bytes, not `atob` chars, and it has to hold a partial sequence
 * until the next frame completes it. Both are invisible to a server test: the
 * bytes on the wire are right either way, and only the rendered text differs.
 *
 * The seam is `globalThis.WebSocket`, as in the machine list test, so the
 * component's real `connectWS` wiring runs against frames this test chooses.
 *
 * Counterfactuals: go back to `this.write(atob(frame.data))` and the accented
 * assertion fails with "hÃ©i"; decode each frame with a fresh `TextDecoder()`
 * (no `stream: true`) and the split case renders two replacement characters.
 */

const assert = {
  equal: (a, b, msg) => {
    if (a !== b) throw new Error(`${msg || 'Not equal'}: expected ${JSON.stringify(b)}, got ${JSON.stringify(a)}`);
  },
  includes: (haystack, needle, msg) => {
    if (!haystack.includes(needle)) throw new Error(`${msg || 'Missing'}: ${JSON.stringify(needle)} in ${JSON.stringify(haystack)}`);
  },
  excludes: (haystack, needle, msg) => {
    if (haystack.includes(needle)) throw new Error(`${msg || 'Unexpected'}: ${JSON.stringify(needle)} in ${JSON.stringify(haystack)}`);
  },
};

let sockets = [];
const RealWebSocket = globalThis.WebSocket;

class FakeSocket {
  static OPEN = 1;
  constructor(url) {
    this.url = url;
    this.readyState = 1;
    this.sent = [];
    this.closed = false;
    sockets.push(this);
    queueMicrotask(() => this.onopen?.({}));
  }
  send(data) {
    this.sent.push(data);
  }
  close() {
    this.closed = true;
    this.onclose = null;
  }
  deliver(message) {
    this.onmessage?.({ data: JSON.stringify(message) });
  }
}

/** Base64 of raw bytes, the way the route encodes a chunk. */
function b64(bytes) {
  return btoa(String.fromCharCode(...bytes));
}

async function mount() {
  await import('../../../modules/machines/components/exec-console.ts');
  const el = document.createElement('exec-console');
  el.setAttribute('machine-id', 'm1');
  document.body.appendChild(el);
  await el.updateComplete;
  return el;
}

/** Type a command and submit the form, then let the fake socket open. */
async function run(el, command) {
  el.querySelector('input').value = command;
  el.querySelector('form').dispatchEvent(new Event('submit', { cancelable: true, bubbles: true }));
  await el.updateComplete;
  await new Promise((resolve) => queueMicrotask(resolve));
  return sockets.at(-1);
}

function output(el) {
  return el.querySelector('pre').textContent;
}

suite('exec-console', () => {
  setup(() => {
    sockets = [];
    globalThis.WebSocket = FakeSocket;
  });

  teardown(() => {
    globalThis.WebSocket = RealWebSocket;
    document.body.innerHTML = '';
  });

  test('sends the command once the socket opens', async () => {
    const el = await mount();
    const ws = await run(el, 'ls');
    assert.equal(ws.sent.length, 1);
    assert.equal(JSON.parse(ws.sent[0]).cmd.join(' '), 'sh -c ls');
    el.remove();
  });

  test('non-ASCII output renders as text, not as one char per byte', async () => {
    const el = await mount();
    const ws = await run(el, 'ls');
    ws.deliver({ type: 'stdout', data: b64(new TextEncoder().encode('café ─ “quote”\n')) });
    ws.deliver({ type: 'exit', code: 0 });
    assert.includes(output(el), 'café ─ “quote”');
    assert.excludes(output(el), 'Ã');
    el.remove();
  });

  test('a multi-byte character split across two frames is joined', async () => {
    const el = await mount();
    const ws = await run(el, 'ls');
    // "héi": é is C3 A9, and the frame boundary falls between the two bytes.
    ws.deliver({ type: 'stdout', data: b64([0x68, 0xc3]) });
    ws.deliver({ type: 'stdout', data: b64([0xa9, 0x69]) });
    ws.deliver({ type: 'exit', code: 0 });
    assert.includes(output(el), 'héi');
    assert.excludes(output(el), '�', 'no replacement character');
    el.remove();
  });

  test('stderr interleaved between two halves of a stdout character does not corrupt it', async () => {
    const el = await mount();
    const ws = await run(el, 'ls');
    // "─" is E2 94 80; stderr lands between its second and third bytes.
    ws.deliver({ type: 'stdout', data: b64([0xe2, 0x94]) });
    ws.deliver({ type: 'stderr', data: b64(new TextEncoder().encode('warn\n')) });
    ws.deliver({ type: 'stdout', data: b64([0x80, 0x0a]) });
    ws.deliver({ type: 'exit', code: 2 });
    const text = output(el);
    assert.includes(text, 'warn');
    assert.includes(text, '─');
    assert.excludes(text, '�');
    assert.includes(text, '[exit 2]');
    el.remove();
  });

  test('a sequence left incomplete at exit is flushed rather than lost', async () => {
    const el = await mount();
    const ws = await run(el, 'ls');
    ws.deliver({ type: 'stdout', data: b64([0x6f, 0x6b, 0xc3]) });
    ws.deliver({ type: 'exit', code: 0 });
    const text = output(el);
    assert.includes(text, 'ok�', 'the dangling byte becomes a replacement character, in order');
    assert.includes(text, '[exit 0]');
    el.remove();
  });
});
