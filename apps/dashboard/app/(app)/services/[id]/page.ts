/**
 * One service: releases, deploy, rollback, PR previews, the repo connection.
 *
 * Only the PREVIOUS healthy release gets a rollback button. A release that
 * never passed its health gate was never serving traffic, so rolling "back" to
 * it would be a deploy of something known broken.
 */
import { html, notFound } from '@webjsdev/core';
import type { PageProps } from '@webjsdev/core';
import { requireOrg } from '#modules/auth/session.server.ts';
import { getService } from '#modules/services/queries/get-service.server.ts';
import { deployService } from '#modules/services/actions/deploy-service.server.ts';
import { rollbackService } from '#modules/services/actions/rollback-service.server.ts';
import { patchService } from '#modules/services/actions/patch-service.server.ts';
import { connectRepo } from '#modules/github/actions/connect-repo.server.ts';
import { disconnectRepo } from '#modules/github/actions/disconnect-repo.server.ts';
import { githubAppConfigured } from '#modules/github/app-jwt.server.ts';
import { installUrl } from '#modules/github/installations.server.ts';

export async function generateMetadata({ params }: PageProps) {
  return { title: `Service ${params.id}` };
}

export default async function ServicePage({ params, actionData }: PageProps) {
  const ctx = (await requireOrg())!;
  const detail = await getService({ orgId: ctx.org.id, id: params.id });
  if (!detail) throw notFound();
  const { service, releases, previews, repo } = detail;

  const errors = (actionData as { fieldErrors?: Record<string, string>; error?: string } | undefined) ?? {};
  // The current release is not a rollback target; the newest healthy one
  // BEFORE it is.
  const rollbackTarget = releases.filter((r) => r.healthy && r.id !== service.release_id)[0];
  const appConfigured = githubAppConfigured();

  return html`
    <h1 class="text-2xl font-semibold tracking-tight m-0">${service.name}</h1>
    <p class="text-muted-foreground mt-1 mb-6">
      ${service.url ? html`<a href=${service.url} rel="noopener">${service.url}</a>` : 'No URL yet'}
    </p>

    ${errors.error ? html`<p role="alert" class="text-destructive">${errors.error}</p>` : ''}

    <section class="mb-8">
      <h2 class="text-lg font-medium m-0 mb-2">Scale</h2>
      <form action=${patchService} class="flex flex-wrap items-end gap-3">
        <input type="hidden" name="service" value=${service.id}>
        <label class="text-sm">
          <span class="block text-muted-foreground">Replicas</span>
          <input
            name="replicas"
            type="number"
            min="0"
            max="100"
            value=${String(service.replicas)}
            class="mt-1 w-24 rounded-md border border-border bg-background px-2 py-1"
          >
        </label>
        <button type="submit" class="rounded-md border border-border px-3 py-1.5 text-sm hover:bg-muted">Save</button>
      </form>
      ${errors.fieldErrors?.replicas
        ? html`<p class="text-sm text-destructive">${errors.fieldErrors.replicas}</p>`
        : ''}
    </section>

    <section class="mb-8">
      <h2 class="text-lg font-medium m-0 mb-2">Releases</h2>
      ${releases.length === 0
        ? html`<p class="text-muted-foreground">
            No releases recorded. The engine's release history route is not serving yet.
          </p>`
        : html`
            <div class="overflow-x-auto">
              <table class="w-full text-sm border-collapse">
                <thead>
                  <tr class="text-left text-muted-foreground border-b border-border">
                    <th class="py-2 pr-4 font-medium">Release</th>
                    <th class="py-2 pr-4 font-medium">Health</th>
                    <th class="py-2 pr-4 font-medium">Build</th>
                    <th class="py-2 font-medium"><span class="sr-only">Actions</span></th>
                  </tr>
                </thead>
                <tbody>
                  ${releases.map(
                    (r) => html`
                      <tr class="border-b border-border">
                        <td class="py-2 pr-4 font-mono">
                          ${r.id}${r.id === service.release_id ? html` <span class="text-xs">(current)</span>` : ''}
                        </td>
                        <td class="py-2 pr-4">${r.healthy ? 'healthy' : 'never passed'}</td>
                        <td class="py-2 pr-4 font-mono text-muted-foreground">${r.rootfs_build_id ?? '-'}</td>
                        <td class="py-2 text-right">
                          ${rollbackTarget && r.id === rollbackTarget.id
                            ? html`
                                <form action=${rollbackService}>
                                  <input type="hidden" name="service" value=${service.id}>
                                  <button
                                    type="submit"
                                    class="rounded-md border border-border px-2 py-1 hover:bg-muted"
                                  >
                                    Roll back to this
                                  </button>
                                </form>
                              `
                            : ''}
                        </td>
                      </tr>
                    `,
                  )}
                </tbody>
              </table>
            </div>
          `}
    </section>

    <section class="mb-8">
      <h2 class="text-lg font-medium m-0 mb-2">Deploy</h2>
      <form action=${deployService} class="flex flex-wrap items-end gap-3">
        <input type="hidden" name="service" value=${service.id}>
        <label class="text-sm">
          <span class="block text-muted-foreground">Release</span>
          <input name="release" placeholder="rel_..." class="mt-1 rounded-md border border-border bg-background px-2 py-1 font-mono">
        </label>
        <label class="text-sm">
          <span class="block text-muted-foreground">or Build</span>
          <input name="build" placeholder="bld_..." class="mt-1 rounded-md border border-border bg-background px-2 py-1 font-mono">
        </label>
        <button type="submit" class="rounded-md border border-border px-3 py-1.5 text-sm hover:bg-muted">Deploy</button>
      </form>
    </section>

    <section class="mb-8">
      <h2 class="text-lg font-medium m-0 mb-2">Repository</h2>
      ${repo
        ? html`
            <dl class="grid grid-cols-[max-content_1fr] gap-x-6 gap-y-1 text-sm mb-3">
              <dt class="text-muted-foreground">Repo</dt>
              <dd class="m-0"><a href=${`https://github.com/${repo.repo}`} rel="noopener">${repo.repo}</a></dd>
              <dt class="text-muted-foreground">Branch</dt>
              <dd class="m-0 font-mono">${repo.branch}</dd>
              <dt class="text-muted-foreground">Autodeploy</dt>
              <dd class="m-0">${repo.autodeploy ? 'on' : 'off'}</dd>
              <dt class="text-muted-foreground">App</dt>
              <dd class="m-0">
                ${!appConfigured
                  ? 'App not configured on this fleet'
                  : repo.installationId
                    ? `installed (#${repo.installationId})`
                    : html`not installed on <strong>${repo.repo.split('/')[0]}</strong>, so pushes are ignored -
                        <a href=${installUrl()} rel="noopener">install it</a>`}
              </dd>
            </dl>
            <form action=${disconnectRepo}>
              <input type="hidden" name="service" value=${service.id}>
              <button type="submit" class="rounded-md border border-border px-3 py-1.5 text-sm hover:bg-muted">
                Disconnect
              </button>
            </form>
          `
        : html`
            <form action=${connectRepo} class="flex flex-wrap items-end gap-3">
              <input type="hidden" name="service" value=${service.id}>
              <label class="text-sm">
                <span class="block text-muted-foreground">Repo</span>
                <input name="repo" placeholder="owner/name" required class="mt-1 rounded-md border border-border bg-background px-2 py-1">
              </label>
              <label class="text-sm">
                <span class="block text-muted-foreground">Branch</span>
                <input name="branch" value="main" class="mt-1 rounded-md border border-border bg-background px-2 py-1">
              </label>
              <label class="text-sm flex items-center gap-2">
                <input name="autodeploy" type="checkbox" checked> Autodeploy
              </label>
              <button type="submit" class="rounded-md border border-border px-3 py-1.5 text-sm hover:bg-muted">
                Connect
              </button>
            </form>
            ${errors.fieldErrors?.repo ? html`<p class="text-sm text-destructive">${errors.fieldErrors.repo}</p>` : ''}
          `}
    </section>

    <section>
      <h2 class="text-lg font-medium m-0 mb-2">Pull-request previews</h2>
      ${previews.length === 0
        ? html`<p class="text-muted-foreground">None open.</p>`
        : html`
            <ul class="text-sm list-none p-0 m-0">
              ${previews.map(
                (m) => html`
                  <li class="border-b border-border py-2 flex flex-wrap gap-x-4">
                    <a href=${`/machines/${m.id}`} class="text-foreground">${m.name}</a>
                    <span class="font-mono text-muted-foreground">${m.state}</span>
                    ${m.url ? html`<a href=${m.url} rel="noopener">${m.url}</a>` : ''}
                  </li>
                `,
              )}
            </ul>
          `}
    </section>
  `;
}
