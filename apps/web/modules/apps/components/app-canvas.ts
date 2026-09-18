/**
 * <app-canvas>: the two things the server-drawn canvas cannot do itself.
 *
 * The picture is complete before this runs: the page positions every card
 * and draws every arrow, and this element only wraps that markup. It adds
 * fit-to-width, scaling the stage down when the viewport is narrower than
 * the layout so nothing is cut off, and arrow-key travel between cards so a
 * keyboard reaches the nearest card in a direction rather than tabbing
 * through them in reading order.
 *
 * The stage declares its own size in `data-width` and `data-height`, which
 * is what the scale is computed from. Below the `sm` breakpoint the cards
 * stack in normal flow and no transform is applied.
 */

import { WebComponent, html } from '@webjsdev/core';

const WIDE = '(min-width: 640px)';

export class AppCanvas extends WebComponent {
  #observer: ResizeObserver | null = null;
  #fittedTo = -1;

  #fit = () => {
    const stage = this.querySelector<HTMLElement>('[data-canvas-stage]');
    if (!stage) return;
    // Setting the host's height below is itself a resize, so the observer
    // calls back once more; fitting only when the WIDTH changed is what keeps
    // that from becoming a loop.
    const available = this.clientWidth;
    if (available === this.#fittedTo) return;
    this.#fittedTo = available;
    const width = Number(stage.dataset.width);
    const height = Number(stage.dataset.height);
    if (!width || !height || !matchMedia(WIDE).matches) {
      stage.style.transform = '';
      this.style.height = '';
      return;
    }
    const scale = Math.min(1, available / width);
    stage.style.transformOrigin = 'top left';
    stage.style.transform = `scale(${scale})`;
    // A transform does not change layout, so the host has to shrink with it
    // or the sections below keep the unscaled gap.
    this.style.height = `${Math.ceil(height * scale)}px`;
  };

  #onKeydown = (event: KeyboardEvent) => {
    const from = (event.target as HTMLElement | null)?.closest<HTMLElement>('[data-canvas-card]');
    if (!from) return;
    const direction = DIRECTIONS[event.key];
    if (!direction) return;
    const next = nearest(from, [...this.querySelectorAll<HTMLElement>('[data-canvas-card]')], direction);
    if (!next) return;
    event.preventDefault();
    next.focus();
  };

  connectedCallback() {
    super.connectedCallback();
    this.addEventListener('keydown', this.#onKeydown);
    // The fit sets the host's own height, which the observer would report in
    // the same frame as a loop; deferring the fit one frame breaks it.
    this.#observer = new ResizeObserver(() => requestAnimationFrame(this.#fit));
    this.#observer.observe(this);
    this.#fit();
  }

  disconnectedCallback() {
    super.disconnectedCallback();
    this.removeEventListener('keydown', this.#onKeydown);
    this.#observer?.disconnect();
    this.#observer = null;
  }

  render() {
    return html`<slot></slot>`;
  }
}
AppCanvas.register('app-canvas');

const DIRECTIONS: Record<string, [number, number]> = {
  ArrowRight: [1, 0],
  ArrowLeft: [-1, 0],
  ArrowDown: [0, 1],
  ArrowUp: [0, -1],
};

/** The closest card whose centre lies in the direction pressed. */
function nearest(from: HTMLElement, cards: HTMLElement[], [dx, dy]: [number, number]): HTMLElement | null {
  const centre = (el: HTMLElement) => {
    const r = el.getBoundingClientRect();
    return { x: r.left + r.width / 2, y: r.top + r.height / 2 };
  };
  const origin = centre(from);
  let best: HTMLElement | null = null;
  let bestScore = Infinity;
  for (const card of cards) {
    if (card === from) continue;
    const c = centre(card);
    const along = (c.x - origin.x) * dx + (c.y - origin.y) * dy;
    if (along <= 1) continue;
    const across = Math.abs((c.x - origin.x) * dy) + Math.abs((c.y - origin.y) * dx);
    // Distance in the pressed direction counts once, drift across it twice,
    // so a card straight ahead beats a nearer one off to the side.
    const score = along + across * 2;
    if (score < bestScore) {
      bestScore = score;
      best = card;
    }
  }
  return best;
}
