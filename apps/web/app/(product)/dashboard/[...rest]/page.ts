/**
 * An address under `/dashboard` that no page matches.
 *
 * Without this the framework falls through to the app's root not-found, which
 * is the marketing 404 (the app has two shells and no root layout, see
 * `app/not-found.ts`). Someone who mistyped `/dashboard/keyz` is in the
 * product, so they get the product's 404 in the product's shell, with "Back to
 * your apps" rather than the marketing home. A catch-all is the lowest-priority
 * match, so every real page still wins.
 */
import { notFound } from '@webjsdev/core';

export default function DashboardNotFound(): never {
  throw notFound();
}
