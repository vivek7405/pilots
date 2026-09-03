/**
 * Browser-safe machine shapes.
 *
 * A component may not value-import `@pilots/sdk` types through a `.server.ts`
 * file, and it does not need the SDK's full `Machine` either: what a row
 * renders is these five fields. So the wire shape is declared here, plainly,
 * with no runtime import of anything server-only.
 */

export interface Machine {
  id: string;
  name?: string;
  state: string;
  host_id?: string;
  url?: string;
  org_id?: string;
}
