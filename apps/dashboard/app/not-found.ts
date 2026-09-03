import { html } from '@webjsdev/core';

export default function NotFound() {
  return html`
    <div class="py-24 text-center">
      <h1 class="text-2xl font-semibold m-0">Not found</h1>
      <p class="text-muted-foreground">That page, machine or service does not exist, or is not yours.</p>
      <a href="/machines" class="text-foreground">Back to machines</a>
    </div>
  `;
}
