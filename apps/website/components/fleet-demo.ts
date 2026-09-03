import { WebComponent, html, signal } from '@webjsdev/core';

/**
 * `<fleet-demo>`: kill a host and watch its machines come back on the
 * survivors.
 *
 * The second claim that prose cannot carry: there is no scheduler to ask where
 * a machine should go. Every surviving host independently computes the same
 * answer from the same rule, so recovery needs no leader, no election, and no
 * human. A reader who kills a host and sees the machines land deterministically
 * has seen why "no central control plane" is a property rather than a slogan.
 *
 * The placement below is the REAL rule, not a plausible-looking stand-in:
 *
 *     hash(machine_id) mod live_hosts == my_index
 *
 * Each survivor rescues exactly the slice that hashes to its own index. Run it
 * on every host and the slices tile the dead host's machines with no overlap
 * and no gap, which is what removes the need to coordinate. Change the rule
 * here and the demo stops teaching the system.
 */

type Machine = { id: string; home: number };

const HOST_NAMES = ['fsn1-a', 'fsn1-b', 'nbg1-a'];

const MACHINES: Machine[] = [
  { id: 'bold-otter', home: 0 },
  { id: 'quiet-finch', home: 0 },
  { id: 'lucky-moth', home: 1 },
  { id: 'plain-heron', home: 1 },
  { id: 'brisk-vole', home: 2 },
  { id: 'north-elk', home: 2 },
];

/**
 * FNV-1a, 32-bit. Any stable hash works; what matters is that every host
 * computes the SAME one, which is why it is a pure function of the id and
 * carries no host state.
 */
function hash(id: string): number {
  let h = 0x811c9dc5;
  for (let i = 0; i < id.length; i++) {
    h ^= id.charCodeAt(i);
    h = Math.imul(h, 0x01000193) >>> 0;
  }
  return h >>> 0;
}

export class FleetDemo extends WebComponent {
  /** Indices of hosts that are down. */
  dead = signal<number[]>([]);

  private isDead(i: number) {
    return this.dead.get().includes(i);
  }

  private toggle(i: number) {
    const dead = this.dead.get();
    if (dead.includes(i)) {
      this.dead.set(dead.filter((d) => d !== i));
      return;
    }
    // Refuse to kill the last host. A fleet of zero has nowhere to rescue TO,
    // and showing machines vanish would teach the opposite of the point.
    if (dead.length >= HOST_NAMES.length - 1) return;
    this.dead.set([...dead, i]);
  }

  /** Where each machine lives right now, after applying the rescue rule. */
  private placement(): Map<string, number> {
    const live = HOST_NAMES.map((_, i) => i).filter((i) => !this.isDead(i));
    const out = new Map<string, number>();
    for (const m of MACHINES) {
      if (!this.isDead(m.home)) {
        out.set(m.id, m.home);
        continue;
      }
      // The dead host's slice, tiled across survivors by the same rule every
      // survivor runs independently.
      out.set(m.id, live[hash(m.id) % live.length]);
    }
    return out;
  }

  render() {
    const place = this.placement();
    const deadCount = this.dead.get().length;

    return html`
      <div class="flex flex-col gap-4">
        <div class="grid gap-3 mid:grid-cols-3">
          ${HOST_NAMES.map((name, i) => {
            const down = this.isDead(i);
            const here = MACHINES.filter((m) => place.get(m.id) === i);
            const canKill = down || deadCount < HOST_NAMES.length - 1;
            return html`
              <div
                class="rounded border p-3 flex flex-col gap-3 transition-colors duration-200
                       ${down ? 'border-rule bg-paper-sunken' : 'border-rule-strong bg-paper-elev'}"
              >
                <div class="flex items-center gap-2">
                  <span
                    class="w-1.5 h-1.5 rounded-full shrink-0 ${down ? 'bg-alert' : 'bg-signal live-dot'}"
                    aria-hidden="true"
                  ></span>
                  <span class="font-mono text-[13px] ${down ? 'text-ink-subtle line-through' : 'text-ink'}"
                    >${name}</span
                  >
                  <button
                    class="ml-auto font-mono text-[11px] px-2 h-6 rounded-[3px] border transition-colors duration-150
                           ${canKill
                             ? 'border-rule text-ink-muted hover:text-ink hover:border-rule-strong cursor-pointer'
                             : 'border-rule text-ink-subtle cursor-not-allowed'}"
                    ?disabled=${!canKill}
                    @click=${() => this.toggle(i)}
                  >${down ? 'revive' : 'kill -9'}</button>
                </div>

                <ul class="m-0 p-0 list-none flex flex-col gap-1 min-h-[4.5rem]">
                  ${here.map((m) => {
                    const rescued = m.home !== i;
                    return html`
                      <li
                        class="font-mono text-[12px] px-2 py-1 rounded-[2px] border
                               ${rescued
                                 ? 'border-signal bg-signal-tint text-ink'
                                 : 'border-rule text-ink-muted'}"
                      >
                        ${m.id}${rescued ? html`<span class="text-ink-subtle"> rescued</span>` : ''}
                      </li>
                    `;
                  })}
                  ${down && here.length === 0
                    ? html`<li class="font-mono text-[12px] text-ink-subtle px-2 py-1">no machines</li>`
                    : ''}
                </ul>
              </div>
            `;
          })}
        </div>

        <p class="m-0 font-mono text-[12px] text-ink-muted">
          ${deadCount === 0
            ? 'All hosts healthy. Kill one and its machines return on the survivors, same URLs.'
            : 'Placement recomputed by every survivor independently: hash(machine_id) mod live_hosts. No leader, no election, no human.'}
        </p>
      </div>
    `;
  }
}

FleetDemo.register('fleet-demo');
