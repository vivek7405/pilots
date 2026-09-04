/**
 * ui.ts: the repeated MARKUP chunks, the counterpart to `components/ui/`.
 *
 * The split the kit draws: a repeated PRIMITIVE with variants (a button, an
 * input, a badge) is a CLASS helper under `components/ui/` returning a class
 * string; a repeated markup CHUNK (a page heading, an empty state, a whole
 * data table) is an `html` fragment here. Both render at SSR time and neither
 * ships any JavaScript, so a page built from them costs the browser nothing.
 *
 * Everything here earns its place by repeating across pages. A one-off stays
 * inline at its call site, where it reads better.
 */

import { html } from '@webjsdev/core';
import type { TemplateResult } from '@webjsdev/core';
import { cn } from '#lib/utils/cn.ts';
import { alertClass, alertDescriptionClass } from '#components/ui/alert.ts';
import {
  tableBodyClass,
  tableCaptionClass,
  tableCellClass,
  tableClass,
  tableContainerClass,
  tableHeadClass,
  tableHeaderClass,
  tableRowClass,
} from '#components/ui/table.ts';

/** The `<h1>` every page opens with. */
export function pageHeading(title: unknown): TemplateResult {
  return html`<h1 class="text-2xl font-semibold tracking-tight m-0">${title}</h1>`;
}

/** The muted paragraph under a page heading. Takes a string or an `html` fragment. */
export function lede(content: unknown): TemplateResult {
  return html`<p class="text-muted-foreground mt-1 mb-6">${content}</p>`;
}

/** The `<h2>` that opens a section within a page. */
export function sectionHeading(title: unknown): TemplateResult {
  return html`<h2 class="text-lg font-medium m-0 mb-2">${title}</h2>`;
}

/** What a list renders instead of itself when it has nothing in it. */
export function emptyState(message: unknown): TemplateResult {
  return html`<p class="text-muted-foreground">${message}</p>`;
}

/** The small print under a table or a form, explaining a rule the UI implies. */
export function footnote(content: unknown): TemplateResult {
  return html`<p class="mt-3 text-xs text-muted-foreground">${content}</p>`;
}

/** Horizontal row of form fields ending in a submit button. */
export const formRowClass = (): string => 'flex flex-wrap items-end gap-3';

/**
 * The banner an action's `error` renders into.
 *
 * `role="alert"` is what makes it reach a screen reader at all, and it is the
 * reason this is a helper: five pages render one, and a missing role on any of
 * them would be a silent failure rather than a visible one.
 */
export function errorAlert(message: unknown): TemplateResult {
  return html`
    <div role="alert" class=${cn(alertClass({ variant: 'destructive' }), 'mb-6')}>
      <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">
        <path d="m21.73 18-8-14a2 2 0 0 0-3.48 0l-8 14A2 2 0 0 0 4 21h16a2 2 0 0 0 1.73-3Z" />
        <path d="M12 9v4M12 17h.01" />
      </svg>
      <div data-slot="alert-description" class=${alertDescriptionClass()}>${message}</div>
    </div>
  `;
}

/**
 * A labelled control.
 *
 * `for`/`id` rather than nesting, because `native-select`'s chevron wrapper
 * sits between the label and the control and nesting would put a positioned
 * element inside the label. `id` is required: a control with no label has no
 * accessible name, and this helper exists so no call site can forget one.
 */
export function field(opts: {
  id: string;
  label: unknown;
  control: unknown;
  hint?: unknown;
  error?: unknown;
}): TemplateResult {
  return html`
    <div class="grid gap-1.5">
      <label class="text-sm leading-none font-medium text-muted-foreground" for=${opts.id}>${opts.label}</label>
      ${opts.control}
      ${opts.hint ? html`<p class="m-0 text-xs text-muted-foreground">${opts.hint}</p>` : ''}
      ${opts.error ? html`<p class="m-0 text-sm text-destructive">${opts.error}</p>` : ''}
    </div>
  `;
}

/** One column of a `dataTable`. */
export interface Column<Row> {
  /** The header cell's text. Pair with `headerHidden` for an actions column. */
  header: unknown;
  /** Visually hide the header while leaving it in the accessibility tree. */
  headerHidden?: boolean;
  /** Right-align the header and every cell (numeric columns, row actions). */
  align?: 'right';
  /** Extra classes for every cell in this column. */
  cellClass?: string;
  cell: (row: Row) => unknown;
}

/**
 * A data table.
 *
 * Nine pages render the same shape, so the shape lives here once. That also
 * makes the kit's two accessibility obligations unforgettable rather than
 * per-call-site: every header cell gets `scope="col"`, and the `caption` is
 * required, visually hidden because a heading above the table already names
 * it on every one of those pages.
 */
export function dataTable<Row>(opts: {
  caption: string;
  columns: Column<Row>[];
  rows: readonly Row[];
  /** Extra classes for one row, e.g. dimming a revoked key. */
  rowClass?: (row: Row) => string;
}): TemplateResult {
  const align = (c: Column<Row>) => (c.align === 'right' ? 'text-right' : '');
  return html`
    <div class=${tableContainerClass()}>
      <table class=${tableClass()}>
        <caption class=${cn(tableCaptionClass(), 'sr-only')}>${opts.caption}</caption>
        <thead class=${tableHeaderClass()}>
          <tr class=${tableRowClass()}>
            ${opts.columns.map(
              (c) => html`
                <th scope="col" class=${cn(tableHeadClass(), align(c))}>
                  ${c.headerHidden ? html`<span class="sr-only">${c.header}</span>` : c.header}
                </th>
              `,
            )}
          </tr>
        </thead>
        <tbody class=${tableBodyClass()}>
          ${opts.rows.map(
            (row) => html`
              <tr class=${cn(tableRowClass(), opts.rowClass?.(row))}>
                ${opts.columns.map(
                  (c) => html`<td class=${cn(tableCellClass(), align(c), c.cellClass)}>${c.cell(row)}</td>`,
                )}
              </tr>
            `,
          )}
        </tbody>
      </table>
    </div>
  `;
}
