/**
 * <machine-terminal machine-id="...">: a real terminal on a machine.
 *
 * It talks to `/api/machines/{id}/terminal`, which rides the exec stream's
 * `tty` mode. The emulator is xterm.js, vendored beside this file; see
 * `vendor/README.md` for why that is the one piece of third-party browser code
 * in this app and how to update it.
 *
 * Two things here are load-bearing rather than stylistic.
 *
 * The first message is `open`, and it carries the FITTED rows and columns. A
 * shell reads its window size at startup, so a session opened at 24 by 80 and
 * resized a frame later redraws its opening prompt where the reader can see it.
 *
 * The theme is read from the page's own tokens and re-read when the toggle
 * writes `data-theme`, so a terminal on a dark page is dark. xterm.js takes
 * concrete colours and cannot resolve a custom property itself, which is why
 * this reads computed styles rather than handing it `var(--background)`.
 */

import { WebComponent, html, prop } from '@webjsdev/core';
import { buttonClass } from '#components/ui/button.ts';
import { cn } from '#lib/utils/cn.ts';

import type { Terminal as XTerm } from './vendor/xterm.mjs';
import type { FitAddon } from './vendor/addon-fit.mjs';

/**
 * Which CSS property carries each token to the probe below, and which xterm
 * theme key it becomes.
 *
 * Three different properties because a probe can only hold one value per
 * property, and all three have to be resolved in one pass.
 */
const PROBE = 'background-color: var(--background); color: var(--foreground); border-color: var(--primary)';

/**
 * The selection wash, built here rather than taken from `--primary-tint`.
 *
 * That token is a `color-mix()`, and a resolved `color-mix()` serialises as
 * `color(srgb 0.12 0.31 0.85 / 0.22)`, which xterm's colour parser does not
 * accept. An `rgb()` triple with an alpha added to it is the same colour in a
 * form every parser has understood for twenty years.
 */
function wash(rgb: string, alpha: number): string {
  const parts = rgb.match(/[\d.]+/g);
  if (!parts || parts.length < 3) return rgb;
  return `rgba(${parts[0]}, ${parts[1]}, ${parts[2]}, ${alpha})`;
}

/**
 * Dim, then reset. Written into the screen when the shell ends.
 *
 * Spelled as an escape rather than a literal ESC byte, which is invisible in
 * every editor and diff and survives exactly one careless paste.
 */
const DIM = '\u001b[2m';
const RESET = '\u001b[0m';

export class MachineTerminal extends WebComponent({
  machineId: prop(String),
  status: prop(String, { state: true }),
  message: prop(String, { state: true }),
}) {
  #term: XTerm | null = null;
  #fit: FitAddon | null = null;
  #socket: WebSocket | null = null;
  #screen: HTMLElement | null = null;
  #resizeObserver: ResizeObserver | null = null;
  #themeObserver: MutationObserver | null = null;
  #onData: { dispose(): void } | null = null;

  constructor() {
    super();
    this.machineId = '';
    this.status = 'connecting';
    this.message = '';
  }

  connectedCallback() {
    super.connectedCallback();
    void this.boot();
  }

  disconnectedCallback() {
    super.disconnectedCallback();
    this.teardown();
  }

  /**
   * The emulator is imported dynamically, so its 345 KB is fetched only on the
   * one route that renders a terminal, and only in a browser.
   */
  private async boot(): Promise<void> {
    const [xterm, fitModule] = await Promise.all([
      import('./vendor/xterm.mjs'),
      import('./vendor/addon-fit.mjs'),
    ]);
    // The reader may have navigated away while the module loaded.
    if (!this.isConnected) return;

    await this.updateComplete;
    this.#screen = this.querySelector('[data-terminal-screen]');
    if (!this.#screen) return;

    const term = new xterm.Terminal({
      cursorBlink: true,
      fontFamily: 'var(--font-mono)',
      fontSize: 13,
      scrollback: 5000,
      theme: this.themeColours(),
    });
    const fit = new fitModule.FitAddon();
    term.loadAddon(fit);
    term.open(this.#screen);
    fit.fit();
    this.#term = term;
    this.#fit = fit;

    this.#onData = term.onData((data) => this.send({ type: 'data', data: encodeUtf8(data) }));
    this.#resizeObserver = new ResizeObserver(() => this.refit());
    this.#resizeObserver.observe(this);
    this.#themeObserver = new MutationObserver(() => {
      if (this.#term) this.#term.options.theme = this.themeColours();
    });
    this.#themeObserver.observe(document.documentElement, {
      attributes: true,
      attributeFilter: ['data-theme', 'class'],
    });

    this.connect();
  }

  /**
   * The page's colours, as colours xterm.js can actually use.
   *
   * NOT `getComputedStyle(root).getPropertyValue('--background')`. A custom
   * property is not resolved by that call: it hands back the declaration's own
   * text, which here is the string `light-dark(#ffffff, #16181d)`. xterm cannot
   * parse that and silently falls back to its own black-on-white, which is how
   * a terminal ends up black on a light page.
   *
   * A throwaway element with the tokens applied to real properties is resolved
   * by the engine, `light-dark()` and the current scheme included, so what
   * comes back is an `rgb()` triple.
   */
  private themeColours(): Record<string, string> {
    const probe = document.createElement('span');
    probe.style.cssText = `position: absolute; visibility: hidden; pointer-events: none; ${PROBE}`;
    document.body.appendChild(probe);
    const style = getComputedStyle(probe);
    const theme = {
      background: style.backgroundColor,
      foreground: style.color,
      cursor: style.color,
      selectionBackground: wash(style.borderTopColor, 0.25),
    };
    probe.remove();
    return theme;
  }

  private connect(): void {
    this.status = 'connecting';
    this.message = '';
    const proto = location.protocol === 'https:' ? 'wss:' : 'ws:';
    const socket = new WebSocket(`${proto}//${location.host}/api/machines/${this.machineId}/terminal`);
    this.#socket = socket;

    socket.addEventListener('open', () => {
      if (this.#socket !== socket) return;
      this.status = 'open';
      // The fitted size goes out FIRST, so the shell starts at the right
      // window and never redraws its opening prompt.
      const term = this.#term;
      this.send({ type: 'open', rows: term?.rows ?? 24, cols: term?.cols ?? 80 });
      term?.focus();
    });

    socket.addEventListener('message', (event: MessageEvent) => {
      if (this.#socket !== socket) return;
      this.receive(String(event.data));
    });

    // Every handler checks that this is still THE socket. Reconnect closes the
    // old one and dials immediately, and a close event is delivered a task
    // later: without the guard the dead socket's close would overwrite the new
    // one's `connecting` with `Disconnected`, and its last frames would still
    // be written into the screen.
    socket.addEventListener('close', (event: CloseEvent) => {
      if (this.#socket !== socket) return;
      if (this.status !== 'error') this.status = 'closed';
      if (event.code === 4401) this.message = 'Your session expired. Reload the page to sign in again.';
      else if (event.code === 4404) this.message = 'That machine is gone.';
    });

    socket.addEventListener('error', () => {
      if (this.#socket !== socket) return;
      this.status = 'error';
      if (!this.message) this.message = 'The connection failed.';
    });
  }

  private receive(raw: string): void {
    let frame: { type?: string; data?: string; code?: number; message?: string };
    try {
      frame = JSON.parse(raw) as typeof frame;
    } catch {
      return;
    }
    if (frame.type === 'data' && typeof frame.data === 'string') {
      this.#term?.write(decodeBase64(frame.data));
      return;
    }
    if (frame.type === 'exit') {
      // Written INTO the terminal rather than beside it: the reader is looking
      // at the screen, and a shell that ended is part of what happened on it.
      this.#term?.writeln(`\r\n${DIM}[the shell exited with ${frame.code ?? 0}]${RESET}`);
      this.status = 'closed';
      return;
    }
    if (frame.type === 'error') {
      this.status = 'error';
      this.message = frame.message ?? 'The session failed.';
    }
  }

  private refit(): void {
    if (!this.#fit || !this.#term) return;
    this.#fit.fit();
    this.send({ type: 'resize', cols: this.#term.cols, rows: this.#term.rows });
  }

  private send(message: Record<string, unknown>): void {
    if (this.#socket?.readyState === WebSocket.OPEN) this.#socket.send(JSON.stringify(message));
  }

  private teardown(): void {
    this.#resizeObserver?.disconnect();
    this.#themeObserver?.disconnect();
    this.#onData?.dispose();
    this.#socket?.close();
    this.#term?.dispose();
    this.#resizeObserver = null;
    this.#themeObserver = null;
    this.#onData = null;
    this.#socket = null;
    this.#term = null;
    this.#fit = null;
  }

  private reconnect = (): void => {
    this.#socket?.close();
    this.connect();
  };

  render() {
    const dot =
      this.status === 'open' ? 'bg-primary' : this.status === 'error' ? 'bg-destructive' : 'bg-muted-foreground';
    const label = this.status === 'open' ? 'Connected' : this.status === 'connecting' ? 'Connecting' : 'Disconnected';
    return html`
      <div class="flex h-full flex-col">
        <div class="flex items-center gap-2 border-b border-border px-3 py-2 text-xs">
          <span class=${cn('inline-block size-1.5 rounded-full', dot)} aria-hidden="true"></span>
          <span role="status" aria-live="polite">${label}</span>
          ${this.message ? html`<span class="text-destructive">${this.message}</span>` : ''}
          <button
            type="button"
            class=${cn(buttonClass({ variant: 'outline', size: 'xs' }), 'ml-auto')}
            @click=${this.reconnect}
          >
            Reconnect
          </button>
        </div>
        <div data-terminal-screen class="min-h-0 flex-1 bg-background p-2"></div>
      </div>
    `;
  }
}
MachineTerminal.register('machine-terminal');

/** Keystrokes are UTF-8 before they are base64, or a non-ASCII paste is lost. */
function encodeUtf8(text: string): string {
  const bytes = new TextEncoder().encode(text);
  let binary = '';
  for (const byte of bytes) binary += String.fromCharCode(byte);
  return btoa(binary);
}

function decodeBase64(base64: string): Uint8Array {
  const binary = atob(base64);
  const bytes = new Uint8Array(binary.length);
  for (let i = 0; i < binary.length; i += 1) bytes[i] = binary.charCodeAt(i);
  return bytes;
}
