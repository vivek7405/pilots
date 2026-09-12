/**
 * The seam a payment provider would plug into, and the one implementation
 * that exists.
 *
 * NOTHING here takes payment. `NoneProvider` makes no network call of any
 * kind, reads no key, and needs no configuration, which is the whole point:
 * pilots is meant to be self-hosted on bare metal by someone who is not
 * running a billing integration, and a plan in that deployment is a quota
 * bundle an owner picks. The interface exists so that adding a real provider
 * later is one new file and one line in `paymentProvider()`, rather than a
 * change to every page and action that touches a plan.
 *
 * A server-only utility, with no `'use server'`: only actions and route
 * handlers reach it, never the browser.
 *
 * `setup` returns `null` when there is nothing for the visitor to go and do,
 * which is what "no provider" means. A real one would return the URL to send
 * them to. Returning `null` rather than throwing is what lets
 * `activate-plan.server.ts` have one code path for both.
 */

import type { BillingAccount, Org } from '#db/schema.server.ts';

/** Where a provider wants the visitor sent to finish setting up. */
export interface Redirect {
  url: string;
}

/** What a provider says about an account it holds. */
export interface Status {
  /** `none` when nobody is collecting; otherwise the provider's own word. */
  state: 'none' | 'active' | 'past_due' | 'canceled';
  /** One sentence for the billing card. Never a raw provider code. */
  detail: string;
}

export interface PaymentProvider {
  /** The value written to `billing_accounts.provider`. */
  readonly id: string;
  setup(org: Org): Promise<Redirect | null>;
  status(account: BillingAccount | null): Promise<Status>;
}

/**
 * No payment provider.
 *
 * A plan change takes effect immediately and no money moves. Every method is
 * total and local: there is no failure mode, so no caller needs a fallback.
 */
export const NoneProvider: PaymentProvider = {
  id: 'none',
  async setup() {
    return null;
  },
  async status(account) {
    if (!account) return { state: 'none', detail: 'No billing account. This team is on the free plan.' };
    return {
      state: 'none',
      detail: 'No payment provider is configured, so plan changes take effect immediately.',
    };
  },
};

/**
 * The provider this deployment uses.
 *
 * One implementation and a function that returns it, rather than a registry
 * keyed on an environment variable: a lookup with one entry is a configuration
 * surface that can only be set wrong. The day there is a second provider, this
 * function is where the choice goes.
 */
export function paymentProvider(): PaymentProvider {
  return NoneProvider;
}
