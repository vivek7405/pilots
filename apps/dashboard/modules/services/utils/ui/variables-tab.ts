/**
 * The Variables tab: what the app reads from its environment.
 *
 * Names are listed from this app's own table, values are never shown, because
 * no API returns an environment to any client. Saving replaces the whole set
 * of each kind on hostd and applies on the next deployment, and the form says
 * both before it asks for a confirm. Each kind carries its own remove box:
 * with no way to pre-fill a field, an empty one has to mean "leave this kind
 * alone", so emptying a set is a thing you ask for rather than imply. Everything pilots injects on its own is
 * listed under a collapsible, each with a sentence, so a reader who has never
 * seen this platform knows what `PORT` is for (`rw-28-variables-provided.png`).
 */
import { html } from '@webjsdev/core';
import type { TemplateResult } from '@webjsdev/core';
import type { ServiceVariable } from '#db/schema.server.ts';
import type { TabProps } from '#modules/services/utils/tabs.ts';
import { saveVariables } from '#modules/services/actions/save-variables.server.ts';
import { badgeClass } from '#components/ui/badge.ts';
import { buttonClass } from '#components/ui/button.ts';
import { checkboxClass } from '#components/ui/checkbox.ts';
import { labelClass } from '#components/ui/label.ts';
import { textareaClass } from '#components/ui/textarea.ts';
import { collapsibleClass, collapsibleContentClass, collapsibleTriggerClass } from '#components/ui/collapsible.ts';
import { dataTable, field, footnote, sectionEmpty, sectionGap, sectionHeading } from '#lib/utils/ui.ts';
import { cn } from '#lib/utils/cn.ts';
import '#modules/services/components/add-secret-row.ts';
import '#components/relative-time.ts';

/** What pilots sets in every service, with the sentence a person needs. */
const PROVIDED: { name: string; what: string }[] = [
  { name: 'PORT', what: 'The port your app must listen on. Requests from the URL arrive here.' },
  { name: '<name>.internal', what: 'How the other services in this app reach this one by name, privately.' },
  { name: 'URL', what: 'The public address this service answers at, when it has one.' },
];

/**
 * The per-kind remove box.
 *
 * Removing has to be asked for, because the form cannot show what is already
 * set -- no API returns an environment -- so an empty field means "I am not
 * touching this kind", never "delete it". See `save-variables.server.ts`.
 */
function removeBox(id: string, name: string, label: string): TemplateResult {
  return html`<label class=${cn(labelClass(), 'flex items-center gap-2 font-normal text-muted-foreground')} for=${id}>
    <input id=${id} name=${name} type="checkbox" data-slot="checkbox" class=${checkboxClass()}>
    <span>${label}</span>
  </label>`;
}

export function variablesTab({ detail, back, errors }: TabProps): TemplateResult {
  const { service } = detail;
  const variables = detail.variables ?? [];
  const fieldErrors = errors.fieldErrors ?? {};

  return html`
    <div class=${sectionGap()}>
      <section>
        ${sectionHeading(
          'Variables',
          'What your app reads from its environment. Saving replaces the whole set of each kind and applies on the next deployment.',
        )}
        ${variables.length === 0
          ? sectionEmpty('No variables set from here', { text: 'Add them below', href: '#variables-form' })
          : dataTable<ServiceVariable>({
              caption: 'Variables set from the dashboard, names only',
              rows: variables,
              columns: [
                { header: 'Name', cellClass: 'font-mono', cell: (v) => v.name },
                {
                  header: 'Kind',
                  cell: (v) =>
                    html`<span class=${badgeClass({ variant: v.secret ? 'secondary' : 'outline' })}
                      >${v.secret ? 'Secret' : 'Plain'}</span
                    >`,
                },
                { header: 'Value', cellClass: 'text-muted-foreground', cell: () => '•••••••' },
                {
                  header: 'Updated',
                  cellClass: 'text-muted-foreground',
                  cell: (v) => html`<relative-time datetime=${String(Math.floor(v.updatedAt.getTime() / 1000))}></relative-time>`,
                },
              ],
            })}
        ${footnote('Variables set by pilot deploy are applied but not listed: the API returns no environment to any client.')}
      </section>

      <section id="variables-form">
        ${sectionHeading('Set variables', 'Plain values are visible to anyone who can read this service. A secret is sealed where it runs.')}
        <form action=${saveVariables} class="grid gap-4">
          <input type="hidden" name="service" value=${service.id}>
          <input type="hidden" name="back" value=${back}>

          ${field({
            id: 'env',
            label: 'Plain variables',
            hint: 'One KEY=value per line.',
            error: fieldErrors.env,
            control: html`<textarea
              id="env"
              name="env"
              rows="4"
              placeholder="NODE_ENV=production"
              spellcheck="false"
              class=${cn(textareaClass(), 'font-mono')}
            ></textarea>`,
          })}
          ${removeBox('clear_env', 'clear_env', 'Remove every plain variable set from here')}

          <fieldset class="m-0 grid gap-2 border-0 p-0">
            <legend class=${cn(labelClass(), 'mb-1')}>Secrets</legend>
            ${fieldErrors.secrets ? html`<p class="m-0 text-meta text-destructive" role="alert">${fieldErrors.secrets}</p>` : ''}
            <add-secret-row></add-secret-row>
            ${removeBox('clear_secrets', 'clear_secrets', 'Remove every secret set from here')}
          </fieldset>

          <label class=${cn(labelClass(), 'flex items-start gap-2 font-normal')} for="confirm">
            <input id="confirm" name="confirm" type="checkbox" data-slot="checkbox" class=${cn(checkboxClass(), 'mt-0.5')}>
            <span>
              I understand this replaces every variable of that kind, and that the next
              <code class="font-mono">pilot deploy</code> from a computer holding a different set replaces it again.
            </span>
          </label>
          ${fieldErrors.confirm ? html`<p class="m-0 -mt-2 text-meta text-destructive" role="alert">${fieldErrors.confirm}</p>` : ''}

          <div><button type="submit" class=${buttonClass()}>Save</button></div>
        </form>
      </section>

      <section>
        <details class=${collapsibleClass()}>
          <summary class=${collapsibleTriggerClass()}>${PROVIDED.length} things pilots provides to every service</summary>
          <div class=${collapsibleContentClass()}>
            <dl class="m-0 grid gap-3">
              ${PROVIDED.map(
                (p) => html`
                  <div>
                    <dt class="font-mono text-body">${p.name}</dt>
                    <dd class="m-0 text-meta text-muted-foreground">${p.what}</dd>
                  </div>
                `,
              )}
            </dl>
          </div>
        </details>
      </section>
    </div>
  `;
}
