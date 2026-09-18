/**
 * The dotted ground an app's picture sits on, shared by the thumbnail on the
 * app card and the canvas itself, so the two read as the same surface at
 * two sizes. The dots are the border token, so they follow the theme.
 */
export const stageClass = (): string =>
  'rounded-lg border border-border bg-[radial-gradient(var(--border)_1px,transparent_1px)] bg-[size:14px_14px]';
