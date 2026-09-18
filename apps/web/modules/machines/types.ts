/**
 * Browser-safe machine shapes.
 *
 * A component may not value-import `@pilots/sdk` types through a `.server.ts`
 * file, and it does not need the SDK's full `Machine` either: what a row
 * renders is these few fields. So the wire shape is declared here, plainly,
 * with no runtime import of anything server-only.
 *
 * Everything past `state` is optional because the engine omits what it does
 * not know: a machine that has never started has no `last_start_at`, and a
 * sandbox has no `service_id`.
 */

export interface Machine {
  id: string;
  name?: string;
  state: string;
  host_id?: string;
  url?: string;
  org_id?: string;
  /**
   * How the machine last came up: `restore`, `boot`, or `cold_boot` -- a
   * restore downgraded because no host of its memory image's CPU vendor was
   * alive. A cold boot keeps the URL and the disk and loses everything in
   * memory, which is a change a viewer needs to see.
   */
  last_start?: string;
  /**
   * SECONDS since the epoch, which is what `time.Now().Unix()` returns and
   * what hostd stamps every one of these with. JavaScript dates are
   * milliseconds, so pass one of these through `epochMs` in
   * `lib/utils/time.ts` before it reaches a `Date`. Reading them raw put every
   * timestamp on every page in January 1970.
   */
  last_start_at?: number;
  last_activity?: number;
  created_at?: number;
  vcpus?: number;
  mem_mib?: number;
  image_ref?: string;
  volume_id?: string;
  /** Set on a replica of a service; absent on a sandbox. */
  service_id?: string;
  release_id?: string;
  /** The app group a service belongs to, which is what `<name>.internal` resolves within. */
  app?: string;
  /**
   * Labels attached at create, for finding a machine again: an agent running
   * twenty sandboxes for one task has only name prefixes otherwise. The list's
   * text filter matches them as `key=value`.
   */
  labels?: Record<string, string>;
  /**
   * Who may reach the URL: `public` (the default, and what every URL was
   * before the mode existed) or `org`, which makes the router demand an API
   * key of the owning org. Absent means public.
   */
  url_auth?: 'public' | 'org';
}
