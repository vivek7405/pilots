/**
 * <live-status>: one status line that keeps up with the fleet.
 *
 * Sleeping and Online were true only at the moment the page was rendered. A
 * sandbox list has said otherwise since `<machine-list>` landed; every Apps
 * surface still needed a reload to notice that a machine had woken, which on a
 * platform whose whole pitch is that a sleeping machine wakes on request is
 * the one number a reader is watching.
 *
 * WHY A SLOT, and not rows passed in as a prop. The server already computed
 * this exact line, so the element renders `<slot>` until the feed has told it
 * something, and only then renders its own. That means:
 *
 *   - with scripting off, the page is unchanged: the slot's content IS the
 *     server's line;
 *   - nothing is duplicated into the HTML. A prop carrying the org's machines,
 *     on a page with a card per app, would serialise the same rows a dozen
 *     times for one feed that says the same thing to all of them;
 *   - there is no hydration flash. The server's markup stays up until real
 *     rows replace it, rather than a `0/1` rendered from an empty prop.
 *
 * What it does carry is the little that cannot be recomputed from machines
 * alone: the services this line is about, and their releases, which is what
 * `serviceHealth` reads to tell a deploy in progress from a failed one.
 *
 * The socket is shared. See `modules/machines/live-client.ts`.
 */

import { WebComponent, html, prop } from '@webjsdev/core';
import type { Machine } from '#modules/machines/types.ts';
import type { HealthRelease } from '#modules/services/utils/health.ts';
import type { AppService } from '#modules/apps/utils/apps.ts';
import { groupApps } from '#modules/apps/utils/apps.ts';
import { onlineLine } from '#modules/apps/utils/ui/online-line.ts';
import { serviceStatusLine } from '#modules/services/utils/ui/status-line.ts';
import type { StatusService } from '#modules/services/utils/ui/status-line.ts';
import { serviceStatus } from '#modules/apps/utils/ui/service-state.ts';
import { healthPills } from '#modules/services/utils/ui/health-pills.ts';
import { serviceHealth } from '#modules/services/utils/health.ts';
import { attachedTo, currentReplicas } from '#modules/services/utils/replicas.ts';
import { subscribeMachines } from '#modules/machines/live-client.ts';

export class LiveStatus extends WebComponent({
  /**
   * `app` renders `N/M services online`, `service` the service status line,
   * `card` the canvas card's `Sleeping since ...`, and `pills` the drawer's
   * health badges. Four shapes, one feed -- each rendered by the same function
   * the server used, so none of them can change shape when it goes live.
   */
  kind: prop(String),
  /** For `app`, every service in it. For `service`, the one service. */
  services: prop<(AppService & StatusService)[]>(Array),
  /** Releases per service id, for the health rule and the deployed stamp. */
  releases: prop<Record<string, HealthRelease[]>>(Object),
  rows: prop<Machine[]>(Array, { state: true }),
  /** False until the feed has spoken; the server's markup shows until then. */
  live: prop(Boolean, { state: true }),
}) {
  private stop: (() => void) | null = null;

  constructor() {
    super();
    this.kind = 'app';
    this.services = [];
    this.releases = {};
    this.rows = [];
    this.live = false;
  }

  connectedCallback() {
    super.connectedCallback();
    this.stop = subscribeMachines((machines) => {
      this.rows = machines;
      this.live = true;
    });
  }

  disconnectedCallback() {
    super.disconnectedCallback();
    // A client-router navigation away takes this element's share of the socket
    // with it, and the last one out closes it.
    this.stop?.();
    this.stop = null;
  }

  render() {
    if (!this.live) return html`<slot></slot>`;
    if (this.kind === 'pills') {
      const service = this.services[0];
      if (!service) return html`<slot></slot>`;
      // Renders to nothing when there is nothing to say, which is the point: a
      // service that has just woken must not keep a `Sleeping` badge because
      // the badge was what the server had to draw. `healthPills` says "nothing"
      // with an empty STRING, and an empty template is what clears the element
      // -- returning the string would leave the base class a value it cannot
      // render, and returning the slot would put the stale badge back.
      const pills = healthPills(
        serviceHealth(service, currentReplicas(this.rows, service), this.releases[service.id] ?? []),
      );
      return typeof pills === 'string' ? html`` : pills;
    }
    if (this.kind === 'service' || this.kind === 'card') {
      const service = this.services[0];
      if (!service) return html`<slot></slot>`;
      // The card summarises the state of the instances the engine manages, so
      // it narrows the way the canvas page does when it renders one.
      if (this.kind === 'card') return serviceStatus(currentReplicas(this.rows, service));
      return serviceStatusLine(service, attachedTo(this.rows, service), this.releases[service.id] ?? []);
    }

    // `groupApps` keys by `service.app`, and every service handed to one of
    // these elements shares one. Taking the single group back out is what
    // keeps this line and the server's computed by the same rule.
    const { apps } = groupApps(this.services, this.rows, this.releases);
    const group = apps[0];
    if (!group) return html`<slot></slot>`;
    return onlineLine(group);
  }
}

LiveStatus.register('live-status');
