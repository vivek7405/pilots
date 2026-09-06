/**
 * How a service gets made.
 *
 * The `New service` button used to lead nowhere, because a service needs a
 * build and a browser form has no repository contents to build. This page is
 * the honest version of that button: the one command that does it, the compose
 * file it reads, and what happens next.
 *
 * There is deliberately no `create` button here yet. `POST /v1/services` would
 * accept a service with no release, and that service could not be removed
 * again: hostd serves no `DELETE /v1/services/{id}` (`internal/api/routes.go`)
 * and the SDK has no `services.delete`. A button that can only ever add
 * unremovable rows is worse than no button. The repository path arrives with
 * the plan endpoint, which builds first and creates second.
 */
import { html } from '@webjsdev/core';
import { requireOrg } from '#modules/auth/session.server.ts';
import { cardClass } from '#components/ui/card.ts';
import { lede, pageHeading, sectionHeading, footnote } from '#lib/utils/ui.ts';
import '#components/copy-button.ts';

export const metadata = { title: 'New service' };

const COMPOSE = `services:
  web:
    build: .
    ports: ["8080"]
    environment:
      DATABASE_URL: secret://database-url
`;

function command(text: string) {
  return html`<span class="flex items-center gap-1">
    <code class="font-mono text-meta bg-muted rounded-md px-3 py-2">${text}</code>
    <copy-button value=${text} label="command"></copy-button>
  </span>`;
}

export default async function NewServicePage() {
  const ctx = (await requireOrg())!;

  return html`
    ${pageHeading('New service')}
    ${lede(html`A service is a deploy that carries a build, so it starts in a repository rather than in this page. It
    lands in <strong>${ctx.org.slug}</strong>.`)}

    <div class=${cardClass()}>
      <h2 class="m-0 text-heading font-medium">From a repository</h2>
      <p class="m-0 text-meta text-muted-foreground">
        Run this in a checkout that has a compose file. It builds the image, creates the service, deploys it and
        waits for the health gate to pass.
      </p>
      ${command('pilot deploy')}
    </div>

    <div class=${cardClass()}>
      <h2 class="m-0 text-heading font-medium">The compose file</h2>
      <p class="m-0 text-meta text-muted-foreground">
        <code class="font-mono">compose.pilots.yaml</code> in the repository root. Three things in it decide what runs.
      </p>
      <pre class="m-0 overflow-x-auto rounded-md bg-muted px-3 py-2 text-meta font-mono">${COMPOSE}</pre>
      <ul class="m-0 pl-5 list-disc text-meta text-muted-foreground">
        <li>Each key under <code class="font-mono">services:</code> becomes one service, and they reach each other at
          <code class="font-mono">&lt;name&gt;.internal</code> inside the app group.</li>
        <li>The app must listen on <code class="font-mono">8080</code>. That is the port pilots dials, and it is
          set as <code class="font-mono">PORT</code> in every copy that runs.</li>
        <li>A <code class="font-mono">secret://</code> value is read at deploy from the secrets store, never from this
          file and never from the image.</li>
      </ul>
    </div>

    ${sectionHeading('Already have a service?', 'Deploy again from the service itself rather than from here.')}
    <p class="text-meta text-muted-foreground">
      Connect a repository from that service's own page and every push to its branch deploys it. A sandbox can also be
      promoted into a service, which keeps its URL.
    </p>
    ${footnote(
      'Creating a service from this page needs a build to exist first, so it arrives with the endpoint that plans a repository. Until then the CLI is the one path that cannot leave a service behind with nothing to serve.',
    )}
  `;
}
