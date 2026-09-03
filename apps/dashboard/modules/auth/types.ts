/**
 * Browser-safe auth types. No runtime import from a `.server.ts` file, so a
 * shipping component may import from here.
 */

/** A role on one org. `owner` may mint admin keys and manage membership. */
export type Role = 'owner' | 'member';

/** The org the current request acts as, as a page or component sees it. */
export interface OrgSummary {
  id: string;
  slug: string;
  name: string;
  personal: boolean;
}

/**
 * The session user.
 *
 * Augmenting `AuthUser` types every `auth()` call in the app at once, which is
 * the whole reason the fields are declared in one place rather than cast at
 * each read. `id` is GitHub's numeric id as a string, because that is what the
 * provider's `profile` mapping produces and what `users.github_id` stores.
 */
declare module '@webjsdev/server' {
  interface AuthUser {
    id: string;
    login: string;
    name: string | null;
    email: string | null;
    image: string | null;
  }
}
