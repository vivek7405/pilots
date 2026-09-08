/**
 * <machine-terminal>, in a real DOM.
 *
 * A browser test is REQUIRED and nothing else would do. Everything this
 * component does happens after hydration: it dynamically imports a 345 KB
 * emulator, opens it into an element, measures the element to fit rows and
 * columns, and only then connects. The served bytes are an empty custom
 * element whether any of that works or not.
 *
 * The assertion that matters most is the first message. A shell reads its
 * window size at startup, so the `open` frame has to carry the FITTED size,
 * not a default that a resize corrects a frame later.
 *
 * Counterfactual: send `open` before `fit()` and the size assertion reports 24
 * by 80 on a container that is plainly not that shape.
 */

const assert = {
  ok: (v, msg) => {
    if (!v) throw new Error(msg || 'Expected truthy');
  },
  equal: (a, b, msg) => {
    if (a !== b) throw new Error(msg || `Expected ${b}, got ${a}`);
  },
  includes: (haystack, needle, msg) => {
    if (!haystack.includes(needle)) throw new Error(msg || `Expected to find ${needle} in ${haystack}`);
  },
};

let sockets = [];
const RealWebSocket = globalThis.WebSocket;

/** A socket the test drives, standing in for the route on the other end. */
class FakeSocket {
  static OPEN = 1;
  constructor(url) {
    this.url = url;
    this.readyState = 1;
    this.sent = [];
    this.listeners = {};
    sockets.push(this);
    // The component assigns its listeners synchronously right after
    // construction, so opening on a task is what real timing looks like.
    setTimeout(() => this.fire('open', {}), 0);
  }
  addEventListener(type, fn) {
    (this.listeners[type] ??= []).push(fn);
  }
  fire(type, event) {
    for (const fn of this.listeners[type] ?? []) fn(event);
  }
  send(data) {
    this.sent.push(JSON.parse(data));
  }
  close() {
    this.readyState = 3;
  }
  /** Deliver one server frame. */
  deliver(frame) {
    this.fire('message', { data: JSON.stringify(frame) });
  }
}

function base64(text) {
  const bytes = new TextEncoder().encode(text);
  let binary = '';
  for (const byte of bytes) binary += String.fromCharCode(byte);
  return btoa(binary);
}

/**
 * Wait until `check()` is true, or give up.
 *
 * The budget is generous on purpose. Everything polled here is a dynamic
 * import, a layout measurement or a server round trip, and a cold CI runner is
 * several times slower than a warm laptop. A budget tuned to the laptop turns
 * a slow machine into a red build, which is a test reporting on the runner
 * rather than on the code.
 */
async function until(check) {
  for (let i = 0; i < 500 && !check(); i += 1) await new Promise((r) => setTimeout(r, 10));
  return check();
}

/**
 * Mount the terminal in a container with a real, non-default size, and hand
 * back the socket THIS mount opened.
 *
 * The index matters. A previous test's boot is an async chain that can still
 * be in flight when the next one starts, and reading `sockets[0]` made this
 * file fail about one run in three against a socket nobody in the test had
 * opened.
 */
async function mount() {
  await import('../../../components/terminal/machine-terminal.ts');
  const mine = sockets.length;
  const host = document.createElement('div');
  host.style.cssText = 'width: 800px; height: 400px;';
  const el = document.createElement('machine-terminal');
  el.setAttribute('machine-id', 'm-1');
  el.style.cssText = 'display: block; width: 100%; height: 100%;';
  host.appendChild(el);
  document.body.appendChild(host);

  // The dynamic import of the emulator plus the fit and the connect all land
  // on later ticks; poll rather than guess a delay.
  await until(() => sockets.length > mine);
  await el.updateComplete;
  return { el, host, socket: sockets[mine] };
}

suite('machine-terminal', () => {
  setup(() => {
    sockets = [];
    globalThis.WebSocket = FakeSocket;
    globalThis.WebSocket.OPEN = 1;
  });

  teardown(async () => {
    document.body.innerHTML = '';
    // Let any boot still in flight reach its `isConnected` check before the
    // next test starts, so it cannot open a socket into the next fixture.
    await new Promise((r) => setTimeout(r, 20));
    globalThis.WebSocket = RealWebSocket;
  });

  test('it mounts the emulator and dials this machine', async () => {
    const { el, host, socket } = await mount();

    assert.ok(el.querySelector('.xterm'), 'xterm rendered into the screen element');
    assert.equal(sockets.length, 1, 'exactly one socket');
    assert.includes(String(socket.url), '/api/machines/m-1/terminal');
    host.remove();
  });

  test('the first message carries the FITTED window, not a default', async () => {
    const { host, socket } = await mount();
    // By TYPE, not by index: the ResizeObserver fires as soon as layout
    // settles, so a resize can legitimately reach the socket first.
    const openFrame = () => socket.sent.find((m) => m.type === 'open');
    await until(() => Boolean(openFrame()));

    const open = openFrame();
    assert.ok(open, 'an open frame went out');
    // 800 by 400 CSS pixels is not 80 by 24 cells at any font size, so a
    // component that skipped the fit reports the library default here.
    assert.ok(open.cols > 0 && open.rows > 0, `open carried ${open.cols}x${open.rows}`);
    assert.ok(open.cols !== 80 || open.rows !== 24, `open carried the unfitted default ${open.cols}x${open.rows}`);
    host.remove();
  });

  test('a data frame is written to the screen', async () => {
    const { el, host, socket } = await mount();
    socket.deliver({ type: 'data', data: base64('hello from the guest\r\n') });

    // xterm writes asynchronously through its own parser queue.
    const text = () => el.querySelector('.xterm-rows')?.textContent ?? '';
    await until(() => text().includes('hello'));
    assert.includes(text(), 'hello from the guest');
    host.remove();
  });

  test('a keystroke leaves as a base64 data frame', async () => {
    const { el, host, socket } = await mount();
    await until(() => socket.sent.length > 0);
    const before = socket.sent.length;

    // Through xterm's own key handling rather than a synthetic call, because
    // what is under test is that this component subscribed to the right event.
    // Enter is the reliable one: printable characters go through the browser's
    // composition path, which a dispatched event does not reproduce.
    const textarea = el.querySelector('textarea');
    assert.ok(textarea, 'xterm rendered its input');
    textarea.dispatchEvent(
      new KeyboardEvent('keydown', { key: 'Enter', code: 'Enter', keyCode: 13, bubbles: true, cancelable: true }),
    );

    const found = () => socket.sent.slice(before).find((m) => m.type === 'data');
    await until(() => Boolean(found()));
    const frame = found();
    assert.ok(frame, 'a data frame went out');
    // Carriage return, which is what a terminal sends for Enter.
    assert.equal(atob(frame.data), '\r');
    host.remove();
  });

  test('the status line says whether the session is live', async () => {
    const { el, host, socket } = await mount();
    await until(() => el.textContent.includes('Connected'));
    assert.includes(el.textContent, 'Connected');

    socket.fire('close', { code: 1000 });
    await el.updateComplete;
    assert.includes(el.textContent, 'Disconnected');
    host.remove();
  });

  test('Reconnect says Connecting, and the dead socket cannot speak over it', async () => {
    const { el, host, socket } = await mount();
    await until(() => el.textContent.includes('Connected'));

    // Reconnect closes the live socket and dials immediately, but a close
    // event is delivered a task LATER. Without a guard the dead socket's
    // handler overwrites the new socket's `connecting` with `closed`, so the
    // header reads Disconnected while a session is in fact coming up.
    el.querySelector('button').click();
    await until(() => sockets.length === 2);
    const fresh = sockets[1];
    assert.ok(fresh !== socket, 'a second socket was opened');

    socket.fire('close', { code: 1006 });
    await el.updateComplete;
    assert.includes(el.textContent, 'Connecting', `the old socket's close won: ${el.textContent.trim()}`);

    // And its last frames must not reach the screen either.
    socket.deliver({ type: 'data', data: base64('from the dead socket') });
    await new Promise((r) => setTimeout(r, 50));
    const text = () => el.querySelector('.xterm-rows')?.textContent ?? '';
    assert.ok(!text().includes('from the dead socket'), `the dead socket wrote: ${text()}`);

    // The new socket still works.
    fresh.fire('open', {});
    await el.updateComplete;
    assert.includes(el.textContent, 'Connected');
    host.remove();
  });

  test('an expired session says so rather than failing silently', async () => {
    const { el, host, socket } = await mount();
    socket.fire('close', { code: 4401 });
    await el.updateComplete;
    assert.includes(el.textContent, 'Your session expired');
    host.remove();
  });
});

/**
 * A hidden tab must give the machine back.
 *
 * hostd counts an exec stream as a request in flight, and the autoscaler
 * re-touches any replica carrying traffic every tick, so a shell held open by
 * a page nobody is looking at pinned its machine awake for as long as the tab
 * existed. Verified on a real fleet: a machine page left open kept
 * `pilots_router_inflight` at 1 and `last_activity` under 10s forever, and the
 * machine suspended within a minute of the socket closing.
 *
 * The grace is driven through `hidden-grace-ms` rather than waited out.
 *
 * Counterfactual: drop the visibilitychange listener and the first test below
 * still finds an open socket, which is the bug exactly.
 */
suite('machine-terminal and a hidden tab', () => {
  let visibility = 'visible';
  let originalDescriptor;

  setup(() => {
    sockets = [];
    globalThis.WebSocket = FakeSocket;
    globalThis.WebSocket.OPEN = 1;
    originalDescriptor = Object.getOwnPropertyDescriptor(Document.prototype, 'visibilityState');
    Object.defineProperty(document, 'visibilityState', { configurable: true, get: () => visibility });
  });

  teardown(async () => {
    visibility = 'visible';
    delete document.visibilityState;
    if (originalDescriptor) Object.defineProperty(Document.prototype, 'visibilityState', originalDescriptor);
    document.body.innerHTML = '';
    await new Promise((r) => setTimeout(r, 20));
    globalThis.WebSocket = RealWebSocket;
  });

  const setVisibility = (value) => {
    visibility = value;
    document.dispatchEvent(new Event('visibilitychange'));
  };

  async function mountFast() {
    const host = document.createElement('div');
    host.style.cssText = 'width:640px;height:320px';
    document.body.appendChild(host);
    const el = document.createElement('machine-terminal');
    el.setAttribute('machine-id', 'm-1');
    el.setAttribute('hidden-grace-ms', '5');
    host.appendChild(el);
    await until(() => sockets.length > 0);
    await el.updateComplete;
    return { el, host, socket: sockets[0] };
  }

  test('a tab hidden past the grace closes the shell so the machine can sleep', async () => {
    const { el, host, socket } = await mountFast();
    assert.equal(socket.readyState, 1, 'the shell is open while the tab is watched');

    setVisibility('hidden');
    await until(() => socket.readyState === 3);
    await el.updateComplete;
    assert.equal(el.status, 'released', `status says why: ${el.status}`);
    assert.equal(sockets.length, 1, 'and it did not open another one on the way out');
    host.remove();
  });

  test('coming back starts a new shell, which is what wakes the machine', async () => {
    const { el, host, socket } = await mountFast();
    setVisibility('hidden');
    await until(() => socket.readyState === 3);

    setVisibility('visible');
    await until(() => sockets.length === 2);
    await el.updateComplete;
    assert.includes(String(sockets[1].url), '/api/machines/m-1/terminal', 'it dials the same machine again');
    host.remove();
  });

  test('a glance at another tab inside the grace leaves the session alone', async () => {
    const { el, host, socket } = await mountFast();
    setVisibility('hidden');
    setVisibility('visible');
    await new Promise((r) => setTimeout(r, 40));
    assert.equal(socket.readyState, 1, 'the shell was never dropped');
    assert.equal(sockets.length, 1, 'and nothing reconnected over the top of it');
    assert.equal(el.status, 'open');
    host.remove();
  });
});
