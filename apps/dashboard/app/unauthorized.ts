import { html } from '@webjsdev/core';
import { signInLink } from '#modules/auth/sign-in-link.ts';

export default function Unauthorized() {
  return html`
    <div class="py-24 flex flex-col items-center gap-6 text-center">
      <h1 class="text-2xl font-semibold m-0">Sign in to continue</h1>
      ${signInLink()}
    </div>
  `;
}
