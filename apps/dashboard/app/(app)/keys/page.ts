/**
 * API keys.
 *
 * The banner at the top renders the plaintext from `actionData`, which exists
 * for exactly one render. There is no way to see it again because nothing
 * stored it: the row holds the sha256 the fleet returned.
 */
import { html } from '@webjsdev/core';
import type { PageProps } from '@webjsdev/core';
import { requireOrg } from '#modules/auth/session.server.ts';
import { listKeys } from '#modules/keys/queries/list-keys.server.ts';
import type { KeyRow } from '#modules/keys/queries/list-keys.server.ts';
import { createKey } from '#modules/keys/actions/create-key.server.ts';
import { revokeKey } from '#modules/keys/actions/revoke-key.server.ts';
import { SCOPES } from '#modules/keys/scopes.ts';
import { alertClass, alertDescriptionClass, alertTitleClass } from '#components/ui/alert.ts';
import { badgeClass } from '#components/ui/badge.ts';
import { buttonClass } from '#components/ui/button.ts';
import { checkboxClass } from '#components/ui/checkbox.ts';
import { inputClass } from '#components/ui/input.ts';
import { labelClass } from '#components/ui/label.ts';
import { dataTable, emptyState, errorAlert, field, footnote, formRowClass, lede, pageHeading } from '#lib/utils/ui.ts';
import { cn } from '#lib/utils/cn.ts';

export const metadata = { title: 'Keys' };

export default async function KeysPage({ actionData }: PageProps) {
  const ctx = (await requireOrg())!;
  const keys = await listKeys(ctx.org.id);
  const result =
    (actionData as
      | { data?: { key: string; name: string }; error?: string; fieldErrors?: Record<string, string> }
      | undefined) ?? {};

  return html`
    ${pageHeading('API keys')}
    ${lede('Minted here, verified on every host from its own replica. This dashboard is in no request path.')}

    ${result.data
      ? html`
          <div role="alert" class=${cn(alertClass(), 'mb-6')}>
            <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">
              <path d="M12 22s8-4 8-10V5l-8-3-8 3v7c0 6 8 10 8 10Z" />
            </svg>
            <div data-slot="alert-title" class=${alertTitleClass()}>Copy this key now. It is shown once.</div>
            <div data-slot="alert-description" class=${alertDescriptionClass()}>
              <code class="block w-full break-all font-mono text-sm text-foreground">${result.data.key}</code>
              <span class="text-xs">Only its hash was stored, so it cannot be shown again.</span>
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
      <fieldset class="border-0 p-0 m-0 grid gap-1.5">
        <legend class="text-sm leading-none font-medium text-muted-foreground p-0">Scopes</legend>
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
                  ?disabled=${scope === 'admin' && ctx.role !== 'owner'}
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
      ? emptyState('No keys yet.')
      : html`
          ${dataTable<KeyRow>({
            caption: 'API keys for this organisation',
            rows: keys,
            rowClass: (k) => (k.revokedAt ? 'opacity-50' : ''),
            columns: [
              { header: 'Name', cell: (k) => k.name },
              { header: 'Prefix', cellClass: 'font-mono', cell: (k) => html`${k.prefix}…` },
              { header: 'Scopes', cellClass: 'font-mono', cell: (k) => k.scopes.join(' ') },
              {
                header: 'Created',
                cellClass: 'text-muted-foreground tabular-nums',
                cell: (k) => k.createdAt.toISOString().slice(0, 10),
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
          })}
          ${footnote('A revoked key keeps its row. Deleting it would erase the record that it ever existed.')}
        `}
  `;
}
