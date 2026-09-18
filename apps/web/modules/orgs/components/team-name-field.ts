/**
 * The team name, with the address it will get shown underneath as you type.
 *
 * The field and the preview are ONE component because the preview is derived
 * from the field: an element that reached out to an input a page rendered
 * would be reading markup it does not own, and would break the first time the
 * page moved the input. It owns both, so there is nothing to find.
 *
 * The derivation is `slugify`, the same function the create action runs, so
 * the preview cannot promise an address the create then refuses. What it
 * cannot know is whether the address is already taken: the create appends
 * `-2` in that case, inside the transaction that claims it, and the sentence
 * under the field says so rather than the preview quietly lying.
 *
 * With scripting off the input still renders and still submits -- the server
 * paints this element's markup -- and the preview simply stays on its
 * placeholder. Nothing about creating a team depends on this element running.
 */

import { WebComponent, html } from '@webjsdev/core';
import { inputClass } from '#components/ui/input.ts';
import { slugify } from '#modules/orgs/slug.ts';

class TeamNameField extends WebComponent({
  value: String,
  error: String,
}) {
  constructor() {
    super();
    this.value = '';
    this.error = '';
  }

  render() {
    const slug = slugify(this.value);
    return html`
      <div class="grid gap-1.5">
        <label class="text-meta leading-none font-medium text-muted-foreground" for="team-name">Name</label>
        <input
          id="team-name"
          name="name"
          placeholder="Acme"
          required
          autocomplete="off"
          maxlength="60"
          aria-describedby="team-slug"
          aria-invalid=${this.error ? 'true' : 'false'}
          class=${inputClass()}
          .value=${this.value}
          @input=${(e: Event) => {
            this.value = (e.target as HTMLInputElement).value;
          }}
        >
        <p id="team-slug" class="m-0 text-meta text-muted-foreground">
          Its address will be
          <code class="font-mono">${slug || 'acme'}</code>. If that is taken, a number is added.
        </p>
        ${this.error ? html`<p class="m-0 text-meta text-destructive">${this.error}</p>` : ''}
      </div>
    `;
  }
}

TeamNameField.register('team-name-field');
