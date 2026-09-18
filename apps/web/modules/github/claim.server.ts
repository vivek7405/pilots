/**
 * The ONE place this app claims a repository for an org on the fleet.
 *
 * hostd keeps a `repo_links` row per (org, repository) and reads it from its
 * local replica before it will fetch a repository by name. That row is what
 * lets the org's OWN key -- `pilot deploy`, the SDK, an agent -- build from a
 * repository, and this app writes it with an ADMIN key acting as the org, so
 * hostd's own admin check never refuses what is asked here. Every surface that
 * connects a repository therefore has to come through this function, or the
 * same product action leaves two different fleet states.
 *
 * WHAT THE CHECK PROVES, exactly: that the pilots GitHub App is installed on
 * the repository's OWNER, so this fleet has been given access to that account
 * by someone who administers it.
 *
 * WHAT IT DOES NOT PROVE: that the signed-in visitor is that someone, or that
 * they may read this particular repository. A person in org B can still name a
 * repository belonging to org A when the App is installed on A's account, and
 * this function will claim it for B. Closing that needs the visitor's OWN
 * GitHub identity -- user-scoped OAuth, and a check that the token can admin
 * the repository -- which this app does not have today. What the check does
 * buy is that a claim can only ever name an account the fleet has been
 * deliberately installed on, rather than any string a visitor types.
 *
 * The claim is also PERMANENT: `repo_links` rows are write-once and there is
 * no disconnect route, so a claim written here cannot be taken back except by
 * uninstalling the App from the repository. That is the reason this refuses
 * rather than "connects optimistically and lets the build sort it out".
 */

import { fleetAs } from '#modules/fleet/client.server.ts';
import { installationFor } from '#modules/github/installations.server.ts';

export interface RepoClaim {
  /** Whether a `repo_links` row was written for this org on the fleet. */
  claimed: boolean;
  /** The App's installation on the owner, or null when it has none. */
  installationId: number | null;
  /** Why no claim was written. Set only when `claimed` is false. */
  reason?: string;
}

/**
 * Claims `repo` for `orgId` on the fleet, when the App is installed on its
 * owner. Throws whatever the SDK throws when the fleet refuses; the callers
 * map that the way they map every other fleet failure.
 */
export async function claimRepo(orgId: string, repo: string): Promise<RepoClaim> {
  const owner = repo.split('/')[0] ?? '';
  const installation = await installationFor(owner).catch(() => null);
  if (!installation) {
    return {
      claimed: false,
      installationId: null,
      reason: `The pilots GitHub App is not installed on ${owner}, so this fleet cannot read that repository. Install it, then connect again.`,
    };
  }
  await fleetAs(orgId).repos.connect(repo);
  return { claimed: true, installationId: installation.id };
}
