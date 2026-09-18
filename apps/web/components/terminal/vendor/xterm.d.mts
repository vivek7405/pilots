/**
 * The slice of xterm.js this app uses, declared rather than vendored.
 *
 * Upstream ships a 1,500-line `xterm.d.ts` describing every addon, decoration
 * and parser hook. None of that is reachable from here, and a copy of it would
 * be one more file to keep in step with `xterm.mjs` for no benefit. This
 * declares exactly what `machine-terminal.ts` touches, so a version bump that
 * removes something we use is a type error rather than a runtime one.
 *
 * Keep it in step with the version recorded in README.md.
 */

export interface ITheme {
  background?: string;
  foreground?: string;
  cursor?: string;
  selectionBackground?: string;
}

export interface ITerminalOptions {
  cursorBlink?: boolean;
  fontFamily?: string;
  fontSize?: number;
  scrollback?: number;
  convertEol?: boolean;
  theme?: ITheme;
}

export interface IDisposable {
  dispose(): void;
}

export declare class Terminal {
  constructor(options?: ITerminalOptions);
  readonly rows: number;
  readonly cols: number;
  options: ITerminalOptions & Record<string, unknown>;
  open(parent: HTMLElement): void;
  write(data: string | Uint8Array): void;
  writeln(data: string): void;
  focus(): void;
  dispose(): void;
  loadAddon(addon: unknown): void;
  onData(handler: (data: string) => void): IDisposable;
}
