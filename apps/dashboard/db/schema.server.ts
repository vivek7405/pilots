import { defineRelations } from 'drizzle-orm';
import { unique } from 'drizzle-orm/sqlite-core';
import { table, pk, uuidPk, text, integer, bool, json, timestamp, createdAt, updatedAt, index } from './columns.server.ts';

/**
 * The dashboard's own store. It is SMALL on purpose.
 *
 * Machines, services, volumes, domains and releases are NOT here: those live
 * in the fleet and are read through `@pilots/sdk` on every request. What this
 * database holds is the product half nothing else owns -- who signed in, which
 * org they act as, which keys were minted, what the hosts reported for usage,
 * and which repo is wired to which service.
 *
 * Two shapes are load-bearing:
 *
 *   - `api_keys` stores the sha256 hash hostd returned and NEVER a plaintext.
 *     The dashboard mints keys and never verifies them; every host answers a
 *     request from its own local replica, so this table is metadata for a
 *     human reading a list, not an authentication path.
 *   - a revoked key keeps its row and gets `revokedAt`. A delete would erase
 *     the record that the key ever existed, which is the one thing an operator
 *     reading an incident actually needs.
 */

export const users = table(
  'users',
  {
    id: pk(),
    githubId: text().notNull().unique(),
    login: text().notNull(),
    name: text(),
    email: text(),
    avatarUrl: text(),
    createdAt: createdAt(),
    updatedAt: updatedAt(),
  },
  (t) => [index(t.login)],
);

export const orgs = table(
  'orgs',
  {
    id: uuidPk(),
    slug: text().notNull().unique(),
    name: text().notNull(),
    personal: bool().notNull().default(false),
    ownerId: integer().notNull(),
    createdAt: createdAt(),
  },
  (t) => [index(t.ownerId)],
);

export const memberships = table(
  'memberships',
  {
    id: pk(),
    userId: integer().notNull(),
    orgId: text().notNull(),
    role: text().notNull(),
    createdAt: createdAt(),
  },
  (t) => [unique('memberships_user_id_org_id_unique').on(t.userId, t.orgId), index(t.orgId)],
);

export const apiKeys = table(
  'api_keys',
  {
    id: uuidPk(),
    orgId: text().notNull(),
    name: text().notNull(),
    /** `pilot_` plus the first 8 characters. Display only, never a secret. */
    prefix: text().notNull(),
    /** The sha256 hostd returned. The plaintext is never stored anywhere. */
    hash: text().notNull().unique(),
    scopes: json<string[]>().notNull(),
    /** Null for a key minted by the CLI exchange, which has no session. */
    createdBy: integer(),
    createdAt: createdAt(),
    lastUsedAt: timestamp(),
    revokedAt: timestamp(),
  },
  (t) => [index(t.orgId), index(t.orgId, t.revokedAt)],
);

export const usageSamples = table(
  'usage_samples',
  {
    id: pk(),
    orgId: text().notNull(),
    hostId: text().notNull(),
    windowStart: timestamp().notNull(),
    windowEnd: timestamp().notNull(),
    machineSeconds: integer().notNull(),
    vcpuSeconds: integer().notNull(),
    mibSeconds: integer().notNull(),
    volumeGibSeconds: integer().notNull(),
    createdAt: createdAt(),
  },
  (t) => [
    // The upsert key. A host re-delivering a window it already reported
    // updates the row rather than adding a second one, which is what lets a
    // failed tick simply re-ask from the same watermark.
    unique('usage_samples_host_id_org_id_window_start_unique').on(t.hostId, t.orgId, t.windowStart),
    index(t.orgId, t.windowStart),
    // The watermark read: MAX(window_end) for one host.
    index(t.hostId, t.windowEnd),
  ],
);

export const repoConnections = table(
  'repo_connections',
  {
    id: uuidPk(),
    orgId: text().notNull(),
    serviceId: text().notNull().unique(),
    /** `owner/name`. */
    repo: text().notNull(),
    branch: text().notNull(),
    autodeploy: bool().notNull().default(true),
    /** Null until the GitHub App is installed on the repo's owner. */
    installationId: integer(),
    connectedBy: integer().notNull(),
    createdAt: createdAt(),
    updatedAt: updatedAt(),
  },
  (t) => [index(t.orgId), index(t.repo)],
);

export const relations = defineRelations(
  { users, orgs, memberships, apiKeys, usageSamples, repoConnections },
  (r) => ({
    users: { memberships: r.many.memberships() },
    orgs: {
      memberships: r.many.memberships(),
      apiKeys: r.many.apiKeys(),
      repoConnections: r.many.repoConnections(),
    },
    memberships: {
      user: r.one.users({ from: r.memberships.userId, to: r.users.id }),
      org: r.one.orgs({ from: r.memberships.orgId, to: r.orgs.id }),
    },
    // Drizzle rc.3 builds a `many` from the matching reverse `one`, so each
    // org-owned table declares its own side rather than repeating from/to on
    // the many above. Without it `defineRelations` throws at import, which
    // takes `webjs db generate` and every boot with it.
    apiKeys: { org: r.one.orgs({ from: r.apiKeys.orgId, to: r.orgs.id }) },
    repoConnections: { org: r.one.orgs({ from: r.repoConnections.orgId, to: r.orgs.id }) },
  }),
);

// Derived types, never hand-written.
export type User = typeof users.$inferSelect;
export type Org = typeof orgs.$inferSelect;
export type Membership = typeof memberships.$inferSelect;
export type ApiKey = typeof apiKeys.$inferSelect;
export type UsageSample = typeof usageSamples.$inferSelect;
export type RepoConnection = typeof repoConnections.$inferSelect;
