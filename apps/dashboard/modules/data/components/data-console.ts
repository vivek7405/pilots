/**
 * The query box and the table under it.
 *
 * Read-only is the default and turning it off is a two-step act: the toggle
 * arms it, and running an armed query asks for a confirmation naming the
 * database. A data browser that can drop a table by accident is worse than no
 * data browser, and the enforcement is the engine's own read-only mode on the
 * server, so this toggle is the ASK rather than the guard.
 */
import { WebComponent, html, prop } from '@webjsdev/core';
import type { TemplateResult } from '@webjsdev/core';

interface Result {
  columns?: string[];
  rows?: unknown[][];
  affected?: number;
  truncated?: boolean;
  wrote?: boolean;
  error?: string;
}

export class DataConsole extends WebComponent({
  serviceId: String,
  serviceName: String,
  engine: String,
  placeholder: String,
  needsTarget: Boolean,
  target: String,
  query: String,
  writing: Boolean,
  running: Boolean,
  result: prop<Result | null>(Object),
}) {
  // Defaults in the CONSTRUCTOR, never as class fields: a class-field
  // initializer runs after the base class installs its reactive accessor and
  // overwrites it with a plain own property, so the element renders once and
  // then never again. webjs check enforces this.
  constructor() {
    super();
    this.serviceId = '';
    this.serviceName = '';
    this.engine = '';
    this.placeholder = '';
    this.needsTarget = false;
    this.target = '';
    this.query = '';
    this.writing = false;
    this.running = false;
    this.result = null;
  }

  private async run(event: Event): Promise<void> {
    event.preventDefault();
    if (this.running || this.query.trim() === '') return;
    // The confirmation names the database. "Are you sure?" answers a question
    // nobody asked; the name is what tells somebody they are about to write to
    // the wrong one.
    if (this.writing) {
      const ok = globalThis.confirm(
        `Run this with writing allowed against ${this.serviceName}? It can change or delete data.`,
      );
      if (!ok) return;
    }
    this.running = true;
    this.result = null;
    try {
      const res = await fetch(`/api/services/${encodeURIComponent(this.serviceId)}/data`, {
        method: 'POST',
        headers: { 'content-type': 'application/json' },
        body: JSON.stringify({
          query: this.query,
          ...(this.target ? { target: this.target } : {}),
          ...(this.writing ? { write: true } : {}),
        }),
      });
      this.result = (await res.json()) as Result;
    } catch (err) {
      this.result = { error: err instanceof Error ? err.message : 'the request failed' };
    } finally {
      this.running = false;
    }
  }

  private table(result: Result): TemplateResult {
    const columns = result.columns ?? [];
    const rows = result.rows ?? [];
    if (rows.length === 0) {
      return html`<p class="m-0 text-body text-muted-foreground">
        ${result.affected === undefined
          ? 'No rows.'
          : `Done. ${result.affected} row${result.affected === 1 ? '' : 's'} changed.`}
      </p>`;
    }
    return html`
      <div class="overflow-x-auto rounded-md border border-border">
        <table class="w-full border-collapse text-body">
          <thead>
            <tr class="border-b border-border bg-muted">
              ${columns.map((c) => html`<th class="px-3 py-2 text-left font-medium">${c}</th>`)}
            </tr>
          </thead>
          <tbody>
            ${rows.map(
              (row) => html`<tr class="border-b border-border last:border-0">
                ${row.map((cell) => html`<td class="px-3 py-2 align-top font-mono">${String(cell ?? '')}</td>`)}
              </tr>`,
            )}
          </tbody>
        </table>
      </div>
      ${result.truncated
        ? html`<p class="m-0 text-meta text-muted-foreground">
            Showing the first ${rows.length}. Narrow the query to see the rest.
          </p>`
        : ''}
    `;
  }

  render(): TemplateResult {
    return html`
      <form class="grid gap-3" @submit=${(e: Event) => this.run(e)}>
        ${this.needsTarget
          ? html`<input
              class="h-9 w-full rounded-md border border-input bg-background px-3 text-body"
              placeholder="collection"
              .value=${this.target}
              @input=${(e: Event) => (this.target = (e.target as HTMLInputElement).value)}
            >`
          : ''}
        <textarea
          rows="4"
          class="w-full rounded-md border border-input bg-background p-3 font-mono text-body"
          placeholder=${this.placeholder}
          .value=${this.query}
          @input=${(e: Event) => (this.query = (e.target as HTMLTextAreaElement).value)}
        ></textarea>
        <div class="flex flex-wrap items-center gap-3">
          <button
            type="submit"
            class="h-9 rounded-md bg-primary px-4 text-body font-medium text-primary-foreground disabled:opacity-50"
            ?disabled=${this.running}
          >
            ${this.running ? 'Running' : 'Run'}
          </button>
          <label class="flex items-center gap-2 text-body text-muted-foreground">
            <input
              type="checkbox"
              .checked=${this.writing}
              @change=${(e: Event) => (this.writing = (e.target as HTMLInputElement).checked)}
            >
            Allow writes
          </label>
          ${this.writing
            ? html`<span class="text-meta text-destructive">
                Writing is allowed. This query can change or delete data.
              </span>`
            : html`<span class="text-meta text-muted-foreground">
                Read only. The database itself refuses a write.
              </span>`}
        </div>
      </form>

      ${this.result === null
        ? ''
        : this.result.error
          ? html`<pre
              class="mt-4 overflow-x-auto whitespace-pre-wrap rounded-md border border-destructive bg-background p-3 text-body text-destructive"
            >${this.result.error}</pre>`
          : html`<div class="mt-4 grid gap-2">${this.table(this.result)}</div>`}
    `;
  }
}

customElements.define('data-console', DataConsole);
