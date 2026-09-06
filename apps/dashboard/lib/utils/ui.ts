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
import { buttonClass } from '#components/ui/button.ts';
import { cardClass } from '#components/ui/card.ts';
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
  return html`<h1 class="text-title font-semibold tracking-tight m-0">${title}</h1>`;
}

/**
 * The one or two letters an avatar falls back to when there is no image.
 *
 * A GitHub login is one word far more often than not, so a two-word split
 * would leave most accounts with a single letter. Splitting on the separators
 * a login may legally carry gives `vivek7405` a V and `ada-lovelace` an AL.
 */
export function initials(name: string): string {
  const parts = name.split(/[-_. ]+/).filter(Boolean);
  if (parts.length === 0) return '?';
  const letters = parts.length === 1 ? [parts[0]![0]] : [parts[0]![0], parts[parts.length - 1]![0]];
  return letters.join('').toUpperCase();
}

/** The muted paragraph under a page heading. Takes a string or an `html` fragment. */
export function lede(content: unknown): TemplateResult {
  return html`<p class="text-muted-foreground mt-1 mb-6">${content}</p>`;
}

/**
 * The `<h2>` that opens a section within a page, with the one sentence that
 * says what the section is.
 *
 * The sentence is the point (#85, D4): a heading on its own names a thing,
 * and a reader who has never read this repo needs to be told what the thing
 * is for. The explanation is optional here only until every call site
 * carries one; a section written after this landed passes it.
 */
export function sectionHeading(title: unknown, explanation?: unknown): TemplateResult {
  return html`<div class="mb-3">
    <h2 class="text-heading font-medium m-0">${title}</h2>
    ${explanation ? html`<p class="text-meta text-muted-foreground m-0 mt-0.5">${explanation}</p>` : ''}
  </div>`;
}

/**
 * A section with nothing in it yet: a dashed box with a headline and the
 * one inline fix that ends the emptiness.
 *
 * Section-level, as opposed to `emptyState`, which is the page-level version
 * naming a command. A section inside a panel does not need a card and a
 * copy button; it needs a line and a link.
 */
export function sectionEmpty(headline: unknown, fix: { text: unknown; href: string }): TemplateResult {
  return html`<div class="rounded-lg border border-dashed border-border px-6 py-10 text-center">
    <p class="m-0 text-body font-medium">${headline}</p>
    <p class="m-0 mt-1 text-meta text-muted-foreground"><a href=${fix.href}>${fix.text}</a></p>
  </div>`;
}

/**
 * The interior padding every card body carries.
 *
 * Spacing is a rule, not a per-page choice: cards and sections used to sit
 * flush with no interior padding on some pages and generous padding on
 * others. One helper, one value.
 */
export const cardBody = (): string => 'p-5 sm:p-6';

/** The vertical rhythm between a page's sections. */
export const sectionGap = (): string => 'space-y-8';

/**
 * What a list renders instead of itself when it has nothing in it.
 *
 * An empty state is the first screen a new user sees, and `No services.` tells
 * them nothing about how to stop it being true. Every call site passes the
 * next step: the command to run, a link, or both.
 */
export function emptyState(
  message: unknown,
  next?: { command?: string; href?: string; label?: string },
): TemplateResult {
  if (!next) return html`<p class="text-muted-foreground">${message}</p>`;
  return html`
    <div class=${cn(cardClass(), 'items-start')}>
      <p class="m-0 text-muted-foreground">${message}</p>
      ${next.command
        ? html`<span class="flex items-center gap-1">
            <code class="font-mono text-sm bg-muted rounded-md px-3 py-2">${next.command}</code>
            <copy-button value=${next.command} label="command"></copy-button>
          </span>`
        : ''}
      ${next.href
        ? html`<a href=${next.href} class=${buttonClass({ variant: 'outline', size: 'sm' })}>${next.label ?? 'Start'}</a>`
        : ''}
    </div>
  `;
}

/** The small print under a table or a form, explaining a rule the UI implies. */
export function footnote(content: unknown): TemplateResult {
  return html`<p class="mt-3 text-meta text-muted-foreground">${content}</p>`;
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
  /**
   * Where a row navigates when clicked anywhere but on a control. It emits
   * `data-href`, which `<link-rows>` acts on; the table itself stays inert, so
   * a page that forgets the wrapper simply has non-clickable rows rather than
   * rows that look clickable and are not.
   */
  rowHref?: (row: Row) => string | undefined;
  /** An id, so a `<list-filter for=...>` can find this table's rows. */
  id?: string;
}): TemplateResult {
  const align = (c: Column<Row>) => (c.align === 'right' ? 'text-right' : '');
  return html`
    <div class=${tableContainerClass()}>
      <table id=${opts.id ?? ''} class=${tableClass()}>
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
              <tr
                data-href=${opts.rowHref?.(row) ?? ''}
                class=${cn(tableRowClass(), opts.rowHref?.(row) ? 'cursor-pointer' : '', opts.rowClass?.(row))}
              >
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
