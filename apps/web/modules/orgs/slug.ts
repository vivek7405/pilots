/**
 * How a team's name becomes the word in its address.
 *
 * Browser-safe and shared on purpose: the new-team form previews the slug as
 * the visitor types, and the action that creates the team derives the real
 * one. Two copies of this would let the preview promise an address the create
 * then refuses to hand out.
 *
 * A GitHub login is already slug-shaped, so `slugify` leaves one untouched:
 * that is what lets the sign-in path and this share a single derivation.
 */

/** The longest a slug may be. Long enough for a name, short enough to type. */
export const SLUG_MAX = 40;

/**
 * The address word for a name: lower case, separators collapsed to one dash,
 * and nothing outside `a-z0-9-`.
 *
 * Returns `''` for a name with no usable characters, which the caller treats
 * as "this cannot be a team name" rather than silently inventing one.
 */
export function slugify(name: string): string {
  return name
    .toLowerCase()
    .normalize('NFKD')
    // Drop the combining marks NFKD just split off, so `Zürich` becomes
    // `zurich` rather than `zu-rich`.
    .replace(/[̀-ͯ]/g, '')
    .replace(/[^a-z0-9]+/g, '-')
    .replace(/^-+|-+$/g, '')
    .slice(0, SLUG_MAX)
    .replace(/-+$/g, '');
}
