/**
 * The Data tab: what is actually in the database.
 *
 * Only on a service that carries an engine label, because there is nothing to
 * browse otherwise. The query runs on the server, through a tunnel opened for
 * that one request: the browser sends a query and receives rows, and no
 * connection string ever reaches it.
 *
 * Read only is the default, and the enforcement is the ENGINE's own read-only
 * transaction rather than a filter over the query text. A filter is a denylist
 * that has to understand comments, dollar quoting and multi-statement bodies,
 * and the first statement it misses is the one that drops the table.
 */
import { html } from '@webjsdev/core';
import type { TemplateResult } from '@webjsdev/core';
import type { TabProps } from '#modules/services/utils/tabs.ts';
import { engineOf, ENGINE_HELP } from '#modules/data/engines.ts';
import { sectionEmpty, sectionGap, sectionHeading } from '#lib/utils/ui.ts';
import '#modules/data/components/data-console.ts';
import '#modules/data/components/engine-stats.ts';

export function dataTab({ detail }: TabProps): TemplateResult {
  const { service, replicas } = detail;
  const engine = engineOf(service);
  if (!engine) {
    return sectionEmpty('Not a database', {
      text: 'How to add one',
      href: 'https://github.com/vivek7405/pilots#databases',
    });
  }
  const help = ENGINE_HELP[engine];
  const running = replicas.some((m) => m.state === 'running');

  return html`
    <div class=${sectionGap()}>
      <section>
        ${sectionHeading(
          `${help.label} health`,
          'The numbers only the engine knows, read by running its own client beside it.',
        )}
        ${running
          ? html`<engine-stats service-id=${service.id}></engine-stats>`
          : html`<p class="m-0 text-body text-muted-foreground">
              Nothing is running, so there is nothing to ask.
            </p>`}
      </section>

      <section>
        ${sectionHeading('Data', help.what)}
        ${running
          ? html`<data-console
              service-id=${service.id}
              service-name=${service.name}
              engine=${engine}
              placeholder=${help.placeholder}
              ?needs-target=${help.needsTarget}
            ></data-console>`
          : sectionEmpty('Nothing running to ask', {
              text: 'Deploy it, then come back',
              href: '#deploy',
            })}
      </section>
    </div>
  `;
}
