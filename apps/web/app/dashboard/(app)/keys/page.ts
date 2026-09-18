/**
 * Tokens: what the CLI and the SDKs authenticate with.
 *
 * The banner at the top renders the plaintext from `actionData`, which exists
 * for exactly one render. There is no way to see it again because nothing
 * stored it: the row holds the sha256 the fleet returned.
 *
 * There is no expiry control, deliberately. hostd has no notion of an expiring
 * key -- nothing in its schema or its verification path reads a date -- so a
 * date stored here would be a date nobody enforces and the token would go on
 * working past it. A security control that does not control anything is worse
 * than an absent one.
 */
import { html } from '@webjsdev/core';
import type { PageProps } from '@webjsdev/core';
import { orUnauthorized, requireOrg } from '#modules/auth/session.server.ts';
import { listKeys } from '#modules/keys/queries/list-keys.server.ts';
import type { KeyRow } from '#modules/keys/queries/list-keys.server.ts';
import { createKey } from '#modules/keys/actions/create-key.server.ts';
import { revokeKey } from '#modules/keys/actions/revoke-key.server.ts';
import { SCOPES } from '#modules/keys/scopes.ts';
import { canAdministerOrg } from '#modules/orgs/roles.ts';
import { alertClass, alertDescriptionClass, alertTitleClass } from '#components/ui/alert.ts';
import { badgeClass } from '#components/ui/badge.ts';
import { buttonClass } from '#components/ui/button.ts';
import { checkboxClass } from '#components/ui/checkbox.ts';
import { inputClass } from '#components/ui/input.ts';
import { labelClass } from '#components/ui/label.ts';
import { dataTable, emptyState, errorAlert, field, footnote, formRowClass, lede, pageHeading } from '#lib/utils/ui.ts';
import { cn } from '#lib/utils/cn.ts';
import '#components/copy-button.ts';
import '#components/relative-time.ts';

export const metadata = { title: 'Tokens' };

export default async function KeysPage({ actionData }: PageProps) {
  const ctx = (await requireOrg())!;
  const keys = orUnauthorized(await listKeys());
  // A revoked token is a record, not a listing: it collapses out of the way so
  // an org that rotates often still has a readable page.
  const live = keys.filter((k) => !k.revokedAt);
  const revoked = keys.filter((k) => k.revokedAt);

  /**
   * The prefix is masked because it is ALL there is: the plaintext left the
   * process once and only its hash was stored, so a last-four is impossible.
   * The prefix is enough to tell two tokens apart and not enough to use.
   */
  const tokenTable = (caption: string, rows: KeyRow[]) =>
    dataTable<KeyRow>({
      caption,
      rows,
      rowClass: (k) => (k.revokedAt ? 'opacity-60' : ''),
      columns: [
        { header: 'Name', cell: (k) => k.name },
        {
          header: 'Token',
          cellClass: 'font-mono',
          cell: (k) => html`<span class="flex items-center gap-1">${k.prefix}<span aria-hidden="true">…</span>
            <span class="sr-only">, the rest is not stored</span>
            <copy-button value=${k.prefix} label="token prefix"></copy-button>
          </span>`,
        },
        { header: 'Scopes', cellClass: 'font-mono', cell: (k) => k.scopes.join(' ') },
        {
          header: 'Created',
          cellClass: 'text-muted-foreground',
          cell: (k) => html`<relative-time datetime=${k.createdAt.toISOString()}></relative-time>`,
        },
        {
          header: 'Actions',
          headerHidden: true,
          align: 'right',
          cell: (k) =>
            k.revokedAt
              ? html`<span class=${badgeClass({ variant: 'outline' })}>revoked</span>`
              : html`
                  <form action=${revokeKey}>
                    <input type="hidden" name="id" value=${k.id}>
                    <button type="submit" class=${buttonClass({ variant: 'outline', size: 'sm' })}>Revoke</button>
                  </form>
                `,
        },
      ],
    });
  const result =
    (actionData as
      | { data?: { key: string; name: string }; error?: string; fieldErrors?: Record<string, string> }
      | undefined) ?? {};

  return html`
    ${pageHeading('Tokens')}
    ${lede('Minted here and verified everywhere pilots runs, from a local copy. This page is in no request path, so a token keeps working while it is down.')}

    ${result.data
      ? html`
          <div role="alert" class=${cn(alertClass(), 'mb-6')}>
            <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">
              <path d="M12 22s8-4 8-10V5l-8-3-8 3v7c0 6 8 10 8 10Z" />
            </svg>
            <div data-slot="alert-title" class=${alertTitleClass()}>Copy this key now. It is shown once.</div>
            <div data-slot="alert-description" class=${alertDescriptionClass()}>
              <span class="flex w-full items-start gap-1">
                <code class="min-w-0 flex-1 break-all font-mono text-body text-foreground">${result.data.key}</code>
                <copy-button value=${result.data.key} label="token"></copy-button>
              </span>
              <span class="text-meta">Only its hash was stored, so it cannot be shown again.</span>
              <span class="flex w-full items-start gap-1">
                <code class="min-w-0 flex-1 break-all font-mono text-meta"
                  >PILOT_API_KEY=${result.data.key}</code
                >
                <copy-button value=${`PILOT_API_KEY=${result.data.key}`} label="environment line"></copy-button>
              </span>
            </div>
          </div>
        `
      : ''}
    ${result.error ? errorAlert(result.error) : ''}

    <form action=${createKey} class=${cn(formRowClass(), 'gap-4 mb-8')}>
      ${field({
        id: 'key-name',
        label: 'Name',
        error: result.fieldErrors ? Object.values(result.fieldErrors).join(' ') : undefined,
        control: html`<input
          id="key-name"
          name="name"
          placeholder="ci"
          required
          aria-invalid=${result.fieldErrors ? 'true' : 'false'}
          class=${inputClass()}
        >`,
      })}
      <!-- A fieldset and legend, not a label: a label's for attribute names one
           control, and this names the whole group. The legend carries an
           explicit margin rather than riding a grid gap, because a legend is
           laid out specially and does not take part in its fieldset's grid,
           which left it half a step above the fields beside it. -->
      <fieldset class="border-0 p-0 m-0">
        <legend class="text-meta leading-none font-medium text-muted-foreground p-0 mb-1.5">Scopes</legend>
        <div class="flex items-center gap-4 h-9">
          ${SCOPES.map(
            (scope) => html`
              <label class=${labelClass()} for=${`scope-${scope}`}>
                <input
                  id=${`scope-${scope}`}
                  type="checkbox"
                  name="scopes"
                  value=${scope}
                  data-slot="checkbox"
                  class=${checkboxClass()}
                  ?disabled=${scope === 'admin' && !canAdministerOrg(ctx.role)}
                >
                <span class="font-mono">${scope}</span>
              </label>
            `,
          )}
        </div>
      </fieldset>
      <button type="submit" class=${buttonClass()}>Mint</button>
    </form>

    ${keys.length === 0
      ? emptyState('No tokens yet. One is what the CLI and the SDKs authenticate with.', {
          command: 'pilot login --token <token>',
        })
      : html`
          ${tokenTable('Tokens for this team', live)}
          ${revoked.length > 0
            ? html`
                <details class="mt-6">
                  <summary class="cursor-pointer text-meta text-muted-foreground">
                    ${revoked.length} revoked ${revoked.length === 1 ? 'token' : 'tokens'}
                  </summary>
                  <div class="mt-3">${tokenTable('Revoked tokens for this team', revoked)}</div>
                </details>
              `
            : ''}
          ${footnote('A revoked token keeps its row. Deleting it would erase the record that it ever existed.')}
        `}
  `;
}
