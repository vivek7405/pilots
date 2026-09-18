/**
 * The billing hooks: per-org usage as CSV and JSON.
 *
 * "Billing hooks" means exportable records, and nothing more. There is no
 * price, no invoice and no payment provider here; what an operator or a
 * finance system needs is the metered quantities per org per period, in a
 * format they can load.
 */

import type { UsageSample } from '#db/schema.server.ts';

export const CSV_COLUMNS = [
  'org_id',
  'host_id',
  'window_start',
  'window_end',
  'machine_seconds',
  'vcpu_seconds',
  'mib_seconds',
  'volume_gib_seconds',
] as const;

export interface UsageTotals {
  machine_seconds: number;
  vcpu_seconds: number;
  mib_seconds: number;
  volume_gib_seconds: number;
}

/** RFC 4180 CSV: one header row, one line per sample, timestamps as ISO 8601. */
export function toCsv(rows: UsageSample[]): string {
  const lines = [CSV_COLUMNS.join(',')];
  for (const r of rows) {
    lines.push(
      [
        csvCell(r.orgId),
        csvCell(r.hostId),
        csvCell(iso(r.windowStart)),
        csvCell(iso(r.windowEnd)),
        String(r.machineSeconds),
        String(r.vcpuSeconds),
        String(r.mibSeconds),
        String(r.volumeGibSeconds),
      ].join(','),
    );
  }
  return lines.join('\n') + '\n';
}

/** The JSON export: the rows, plus the totals a reader would otherwise sum. */
export function toJson(rows: UsageSample[]): { samples: unknown[]; totals: UsageTotals } {
  const totals: UsageTotals = { machine_seconds: 0, vcpu_seconds: 0, mib_seconds: 0, volume_gib_seconds: 0 };
  const samples = rows.map((r) => {
    totals.machine_seconds += r.machineSeconds;
    totals.vcpu_seconds += r.vcpuSeconds;
    totals.mib_seconds += r.mibSeconds;
    totals.volume_gib_seconds += r.volumeGibSeconds;
    return {
      org_id: r.orgId,
      host_id: r.hostId,
      window_start: iso(r.windowStart),
      window_end: iso(r.windowEnd),
      machine_seconds: r.machineSeconds,
      vcpu_seconds: r.vcpuSeconds,
      mib_seconds: r.mibSeconds,
      volume_gib_seconds: r.volumeGibSeconds,
    };
  });
  return { samples, totals };
}

/** The filename a browser saves a CSV export under. */
export function csvFilename(orgSlug: string, since: Date, until: Date): string {
  return `usage-${orgSlug}-${day(since)}-${day(until)}.csv`;
}

function iso(value: Date | number): string {
  return (value instanceof Date ? value : new Date(value)).toISOString();
}

function day(value: Date): string {
  return value.toISOString().slice(0, 10);
}

/** Quotes only when a cell would otherwise break the row. */
function csvCell(value: string): string {
  return /[",\n\r]/.test(value) ? `"${value.replace(/"/g, '""')}"` : value;
}
