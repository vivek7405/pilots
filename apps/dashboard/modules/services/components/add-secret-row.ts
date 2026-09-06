/**
 * <add-secret-row>: the Secrets fieldset of the Variables form.
 *
 * It renders its rows on the server so the form works with scripting off, and
 * with scripting on the button adds another. The row count is the only state;
 * the inputs are plain form fields the action reads as parallel lists, so a
 * row is a name beside a password and nothing else.
 */
import { WebComponent, html, prop } from '@webjsdev/core';
import { buttonClass } from '#components/ui/button.ts';
import { inputClass } from '#components/ui/input.ts';
import { cn } from '#lib/utils/cn.ts';

export class AddSecretRow extends WebComponent({ rows: prop(Number, { state: true }) }) {
  constructor() {
    super();
    this.rows = 4;
  }

  private add = () => {
    this.rows += 1;
  };

  render() {
    return html`
      <div class="grid gap-2">
        ${Array.from({ length: this.rows }, (_, i) => html`
          <div class="grid grid-cols-[1fr_1fr] gap-2">
            <input
              name="secret_name"
              placeholder="NAME"
              autocomplete="off"
              spellcheck="false"
              aria-label=${`Secret ${i + 1} name`}
              class=${cn(inputClass(), 'font-mono')}
            />
            <input
              name="secret_value"
              type="password"
              placeholder="value"
              autocomplete="new-password"
              aria-label=${`Secret ${i + 1} value`}
              class=${inputClass()}
            />
          </div>
        `)}
        <div>
          <button type="button" @click=${this.add} class=${buttonClass({ variant: 'outline', size: 'sm' })}>
            Add another secret
          </button>
        </div>
      </div>
    `;
  }
}

AddSecretRow.register('add-secret-row');
