/**
 * Browser-safe fleet shapes.
 *
 * A shipping component may not value-import `@pilots/sdk` through a
 * `.server.ts` file, and it does not need the SDK's full types either. These
 * are the fields the browser actually reads, declared plainly.
 */

export interface Host {
  id: string;
  alive: boolean;
  cpu_free: number;
  mem_free_mib: number;
  /**
   * `AuthenticAMD` or `GenuineIntel`. A memory image is never restored across
   * that boundary, so this is what decides whether a suspended machine resumes
   * warm or has to cold-boot from its own disk.
   */
  cpu_vendor?: string;
}

/** The org's ceilings, as `GET /v1/quotas/{org}` returns them. */
export interface Quota {
  max_machines?: number;
  max_vcpus?: number;
  max_mem_mib?: number;
  max_volume_gib?: number;
  max_builds?: number;
}
