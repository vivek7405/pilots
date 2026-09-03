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
import { createKey } from '#modules/keys/actions/create-key.server.ts';
import { revokeKey } from '#modules/keys/actions/revoke-key.server.ts';
import { SCOPES } from '#modules/keys/scopes.ts';

export const metadata = { title: 'Keys' };

export default async function KeysPage({ actionData }: PageProps) {
  const ctx = (await requireOrg())!;
  const keys = await listKeys(ctx.org.id);
  const result =
    (actionData as
      | { data?: { key: string; name: string }; error?: string; fieldErrors?: Record<string, string> }
      | undefined) ?? {};

  return html`
    <h1 class="text-2xl font-semibold tracking-tight m-0">API keys</h1>
    <p class="text-muted-foreground mt-1 mb-6">
      Minted here, verified on every host from its own replica. This dashboard is in no request path.
    </p>

    ${result.data
      ? html`
          <div role="alert" class="mb-6 rounded-md border border-border bg-muted p-4">
            <p class="m-0 font-medium">Copy this key now. It is shown once.</p>
            <code class="mt-2 block break-all font-mono text-sm">${result.data.key}</code>
            <p class="m-0 mt-2 text-xs text-muted-foreground">
              Only its hash was stored, so it cannot be shown again.
            </p>
          </div>
        `
      : ''}
    ${result.error ? html`<p role="alert" class="text-destructive">${result.error}</p>` : ''}

    <form action=${createKey} class="flex flex-wrap items-end gap-4 mb-8">
      <label class="text-sm">
        <span class="block text-muted-foreground">Name</span>
        <input name="name" placeholder="ci" required class="mt-1 rounded-md border border-border bg-background px-2 py-1">
      </label>
      <fieldset class="border-0 p-0 m-0 text-sm">
        <legend class="text-muted-foreground">Scopes</legend>
        <div class="mt-1 flex gap-4">
          ${SCOPES.map(
            (scope) => html`
              <label class="flex items-center gap-1.5">
                <input
                  type="checkbox"
                  name="scopes"
                  value=${scope}
                  ?disabled=${scope === 'admin' && ctx.role !== 'owner'}
                >
                <span class="font-mono">${scope}</span>
              </label>
            `,
          )}
        </div>
      </fieldset>
      <button type="submit" class="rounded-md border border-border px-3 py-1.5 text-sm hover:bg-muted">Mint</button>
    </form>
    ${result.fieldErrors
      ? html`<p class="text-sm text-destructive">${Object.values(result.fieldErrors).join(' ')}</p>`
      : ''}

    ${keys.length === 0
      ? html`<p class="text-muted-foreground">No keys yet.</p>`
      : html`
          <div class="overflow-x-auto">
            <table class="w-full text-sm border-collapse">
              <thead>
                <tr class="text-left text-muted-foreground border-b border-border">
                  <th class="py-2 pr-4 font-medium">Name</th>
                  <th class="py-2 pr-4 font-medium">Prefix</th>
                  <th class="py-2 pr-4 font-medium">Scopes</th>
                  <th class="py-2 pr-4 font-medium">Created</th>
                  <th class="py-2 font-medium"><span class="sr-only">Actions</span></th>
                </tr>
              </thead>
              <tbody>
                ${keys.map(
                  (k) => html`
                    <tr class=${k.revokedAt ? 'border-b border-border opacity-50' : 'border-b border-border'}>
                      <td class="py-2 pr-4">${k.name}</td>
                      <td class="py-2 pr-4 font-mono">${k.prefix}…</td>
                      <td class="py-2 pr-4 font-mono">${k.scopes.join(' ')}</td>
                      <td class="py-2 pr-4 text-muted-foreground">${k.createdAt.toISOString().slice(0, 10)}</td>
                      <td class="py-2 text-right">
                        ${k.revokedAt
                          ? html`<span class="text-muted-foreground">revoked</span>`
                          : html`
                              <form action=${revokeKey}>
                                <input type="hidden" name="id" value=${k.id}>
                                <button type="submit" class="rounded-md border border-border px-2 py-1 hover:bg-muted">
                                  Revoke
                                </button>
                              </form>
                            `}
                      </td>
                    </tr>
                  `,
                )}
              </tbody>
            </table>
          </div>
          <p class="mt-3 text-xs text-muted-foreground">
            A revoked key keeps its row. Deleting it would erase the record that it ever existed.
          </p>
        `}
  `;
}
