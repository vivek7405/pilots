/**
 * New app: point pilots at a GitHub repository and deploy it to a URL.
 *
 * Two forms. The first is a plain GET carrying the repository and ref, so the
 * page plans during render and shows what it found; nothing is created by
 * looking. The second is bound to the create action and appears only when the
 * plan is one service pilots can deploy from here: it starts the build,
 * creates the service, and lands on the deployments view that follows the
 * build to its verdict. Everything else the plan can say has its own render:
 * no GitHub App on this fleet, a repository nothing recognises, or a plan the
 * CLI must run because it has several services or needs storage.
 *
 * The create button used to be withheld because a service with no release
 * could never be removed. It exists now because it creates a service with a
 * build in flight and a connected repository, retried by a push or by
 * `pilot deploy`, which is the same leftover a CLI deploy leaves.
 */
import { html } from '@webjsdev/core';
import type { PageProps } from '@webjsdev/core';
import type { ComposeUnknownDetails } from '@pilots/sdk';
import { requireOrg } from '#modules/auth/session.server.ts';
import { planRepo } from '#modules/apps/queries/plan-repo.server.ts';
import { createFromRepo } from '#modules/apps/actions/create-from-repo.server.ts';
import { installationFor, installUrl } from '#modules/github/installations.server.ts';
import { isRepoSlug } from '#modules/domains/hostname.ts';
import { buttonClass } from '#components/ui/button.ts';
import { cardClass } from '#components/ui/card.ts';
import { inputClass } from '#components/ui/input.ts';
import { cardBody, errorAlert, field, formRowClass, lede, pageHeading, sectionEmpty, sectionGap, sectionHeading } from '#lib/utils/ui.ts';
import { cn } from '#lib/utils/cn.ts';
import '#components/copy-button.ts';

export const metadata = { title: 'New app' };

function command(text: string) {
  return html`<span class="flex items-center gap-1">
    <code class="font-mono text-meta bg-muted rounded-md px-3 py-2">${text}</code>
    <copy-button value=${text} label="command"></copy-button>
  </span>`;
}

/** The fallback every refusal ends in: the CLI, run in a checkout. */
function cliBlock(why: unknown) {
  return html`
    <div class=${cn(cardClass(), 'gap-0 py-0')}>
      <div class=${cn(cardBody(), 'grid gap-2')}>
        <p class="m-0 text-body">${why}</p>
        <p class="m-0 text-meta text-muted-foreground">Run this in a checkout of the repository:</p>
        ${command('pilot deploy')}
      </div>
    </div>
  `;
}

export default async function NewAppPage({ searchParams, actionData }: PageProps) {
  const ctx = (await requireOrg())!;
  const repo = typeof searchParams.repo === 'string' ? searchParams.repo.trim() : '';
  const ref = (typeof searchParams.ref === 'string' && searchParams.ref.trim()) || 'main';
  const app = (typeof searchParams.app === 'string' && searchParams.app.trim()) || (repo.split('/')[1] ?? '');
  const errors = (actionData as { fieldErrors?: Record<string, string>; error?: string } | undefined) ?? {};
  const fieldErrors = errors.fieldErrors ?? {};

  const lookForm = html`
    <form method="get" action="/services/new" class="grid gap-3">
      ${field({
        id: 'repo',
        label: 'Repository',
        hint: 'owner/name on GitHub',
        error: fieldErrors.repo,
        control: html`<input id="repo" name="repo" value=${repo} placeholder="acme/shop" required class=${cn(inputClass(), 'font-mono')}>`,
      })}
      <div class=${formRowClass()}>
        ${field({
          id: 'ref',
          label: 'Branch or tag',
          control: html`<input id="ref" name="ref" value=${ref} class=${cn(inputClass(), 'font-mono')}>`,
        })}
        ${field({
          id: 'app',
          label: 'App',
          hint: 'Services in one app reach each other by name.',
          control: html`<input id="app" name="app" value=${app} placeholder=${repo.split('/')[1] ?? 'shop'} class=${inputClass()}>`,
        })}
      </div>
      <div><button type="submit" class=${buttonClass()}>Look inside</button></div>
    </form>
  `;

  let result: unknown = '';
  if (repo) {
    if (!isRepoSlug(repo)) {
      result = sectionEmpty('That is not a repository', { text: 'Use the owner/name form, like acme/shop.', href: '/services/new' });
    } else {
      const owner = repo.split('/')[0];
      const installation = await installationFor(owner).catch(() => null);
      if (!installation) {
        result = html`
          <div class=${cn(cardClass(), 'gap-0 py-0')}>
            <div class=${cn(cardBody(), 'grid gap-2')}>
              <p class="m-0 text-body">pilots is not installed on <strong>${owner}</strong> yet, so it cannot read the repository.</p>
              <div><a href=${installUrl()} rel="noopener" class=${cn(buttonClass(), 'no-underline')}>Install the pilots app on ${owner}</a></div>
            </div>
          </div>
        `;
      } else {
        const outcome = await planRepo({ repo, ref, app });
        if (!outcome) {
          result = '';
        } else if (!outcome.ok) {
          const { code, message, next, details } = outcome.refusal;
          if (code === 'not_configured') {
            result = cliBlock('This fleet has no GitHub App, so the host cannot fetch a repository on its own.');
          } else if (code === 'unknown_framework') {
            const d = (details ?? {}) as Partial<ComposeUnknownDetails>;
            result = html`
              <div class=${cn(cardClass(), 'gap-0 py-0')}>
                <div class=${cn(cardBody(), 'grid gap-3')}>
                  <p class="m-0 text-body">pilots could not tell how to build <strong>${repo}</strong>. Add a Dockerfile, then try again.</p>
                  ${d.rules?.length
                    ? html`<div>
                        <p class="m-0 text-meta text-muted-foreground">What it looked for:</p>
                        <ul class="m-0 pl-5 list-disc text-meta">${d.rules.map((r) => html`<li>${r}</li>`)}</ul>
                      </div>`
                    : ''}
                  ${d.listing?.length
                    ? html`<div>
                        <p class="m-0 text-meta text-muted-foreground">What is in <code class="font-mono">${d.dir || '/'}</code>:</p>
                        <pre class="m-0 overflow-x-auto rounded-md bg-muted px-3 py-2 text-meta font-mono">${d.listing.join('\n')}</pre>
                      </div>`
                    : ''}
                </div>
              </div>
            `;
          } else {
            result = cliBlock(html`${message}${next ? html` <span class="text-muted-foreground">${next}</span>` : ''}`);
          }
        } else {
          const { plan, detected } = outcome.plan;
          const steps = plan.steps;
          if (steps.length !== 1 || (steps[0].volumes?.length ?? 0) > 0) {
            result = html`
              <div class=${sectionGap()}>
                ${sectionHeading(
                  steps.length === 1 ? 'This service needs storage' : `${steps.length} services found`,
                  'The dashboard deploys one service without storage. The CLI deploys the rest, in the right order.',
                )}
                <ul class="m-0 pl-5 list-disc text-body">
                  ${steps.map((s) => html`<li><span class="font-mono">${s.name}</span>${s.volumes?.length ? ' (with storage)' : ''}</li>`)}
                </ul>
                ${cliBlock('Deploy it from a checkout instead:')}
              </div>
            `;
          } else {
            const step = steps[0];
            const found = detected[0];
            const source =
              found?.source === 'compose'
                ? 'a compose file'
                : found?.source === 'dockerfile'
                  ? 'a Dockerfile'
                  : found?.framework
                    ? `a ${found.framework} app`
                    : 'the repository';
            const secrets = Object.keys(step.secret_refs ?? {});
            result = html`
              <div class=${cn(cardClass(), 'gap-0 py-0')}>
                <div class=${cn(cardBody(), 'grid gap-4')}>
                  <div>
                    <h2 class="m-0 text-heading font-medium">Ready to deploy</h2>
                    <p class="m-0 mt-1 text-meta text-muted-foreground">
                      pilots found ${source} in <strong>${repo}</strong>. It listens on
                      <code class="font-mono">${found?.port ?? step.ports?.[0] ?? 8080}</code>${step.health?.path
                        ? html`, answers health at <code class="font-mono">${step.health.path}</code>`
                        : ''}${step.env && Object.keys(step.env).length
                        ? html`, and reads ${Object.keys(step.env).map((k) => html`<code class="font-mono">${k}</code>`)}`
                        : ''}.
                    </p>
                  </div>
                  <form action=${createFromRepo} class="grid gap-3">
                    <input type="hidden" name="repo" value=${repo}>
                    <input type="hidden" name="ref" value=${ref}>
                    <input type="hidden" name="app" value=${app}>
                    <div class=${formRowClass()}>
                      ${field({
                        id: 'name',
                        label: 'Service name',
                        error: fieldErrors.name,
                        control: html`<input id="name" name="name" value=${step.name} required class=${cn(inputClass(), 'font-mono')}>`,
                      })}
                      ${field({
                        id: 'domain',
                        label: 'Address',
                        hint: 'Becomes <address>.pilotrun.app; leave it empty to use the service name',
                        error: fieldErrors.domain,
                        control: html`<input id="domain" name="domain" value=${step.name} class=${cn(inputClass(), 'font-mono')}>`,
                      })}
                    </div>
                    ${secrets.length
                      ? html`<fieldset class="m-0 grid gap-2 border-0 p-0">
                          <legend class="text-body font-medium mb-1">Secrets this service needs</legend>
                          ${secrets.map((key) =>
                            field({
                              id: `secret-${key}`,
                              label: html`<span class="font-mono">${key}</span>`,
                              error: fieldErrors[`secret:${key}`],
                              control: html`<input id=${`secret-${key}`} name=${`secret:${key}`} type="password" required autocomplete="new-password" class=${inputClass()}>`,
                            }),
                          )}
                        </fieldset>`
                      : ''}
                    <div><button type="submit" class=${buttonClass()}>Deploy</button></div>
                  </form>
                </div>
              </div>
            `;
          }
        }
      }
    }
  }

  return html`
    ${pageHeading('New app')}
    ${lede(html`Point pilots at a GitHub repository. It works out how to build it, shows you what it found, and deploys it to a URL, in <strong>${ctx.org.slug}</strong>.`)}
    <div class=${sectionGap()}>
      ${errors.error ? errorAlert(errors.error) : ''}
      <section>${lookForm}</section>
      ${result ? html`<section>${result}</section>` : ''}
    </div>
  `;
}
