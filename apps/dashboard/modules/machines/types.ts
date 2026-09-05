/**
 * Browser-safe machine shapes.
 *
 * A component may not value-import `@pilots/sdk` types through a `.server.ts`
 * file, and it does not need the SDK's full `Machine` either: what a row
 * renders is these few fields. So the wire shape is declared here, plainly,
 * with no runtime import of anything server-only.
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
}
