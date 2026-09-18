/**
 * <live-machine-state>: one machine's state, kept current.
 *
 * `<live-status>` is about a SERVICE -- a count, a verdict, a summary over its
 * instances. This is the row-level companion: the instance lists in the
 * service drawer name one machine each, and each says `Sleeping since 2 hours`
 * or carries a coloured dot. Those were true at render and then stopped being
 * true, which is what made the drawer disagree with the card that opened it.
 *
 * Same contract as `<live-status>`: `<slot>` until the feed speaks, so the
 * server's markup is what a browser with no scripting shows and there is no
 * flash; the socket is the shared one; the renderers are the same functions
 * the server called, so a row cannot change shape when it goes live.
 *
 * A machine the feed does not mention keeps showing the server's markup rather
 * than rendering as gone. Membership of these lists is decided by the server
 * -- an instance ADDED after the page loaded still needs a reload to appear --
 * and a row that erased itself on a snapshot that simply arrived before the
 * new machine did would be worse than one that is briefly stale.
 */

import { WebComponent, html, prop } from '@webjsdev/core';
import type { Machine } from '#modules/machines/types.ts';
import { statusDot } from '#modules/machines/utils/ui/state.ts';
import { machineStateSince } from '#modules/machines/utils/ui/status-line.ts';
import { subscribeMachines } from '#modules/machines/live-client.ts';

export class LiveMachineState extends WebComponent({
  machineId: prop(String, { attribute: 'machine-id' }),
  /** `since` renders the state and how long it has held; `dot` the badge. */
  mode: prop(String),
  rows: prop<Machine[]>(Array, { state: true }),
  live: prop(Boolean, { state: true }),
}) {
  private stop: (() => void) | null = null;

  constructor() {
    super();
    this.machineId = '';
    this.mode = 'since';
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
    this.stop?.();
    this.stop = null;
  }

  render() {
    if (!this.live) return html`<slot></slot>`;
    const machine = this.rows.find((m) => m.id === this.machineId);
    if (!machine) return html`<slot></slot>`;
    return this.mode === 'dot' ? statusDot(machine.state) : machineStateSince(machine);
  }
}

LiveMachineState.register('live-machine-state');
