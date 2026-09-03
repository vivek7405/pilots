/**
 * `pilot add postgres`: a database as a compose fragment on ordinary primitives.
 *
 * There is no Postgres-as-a-service here and there is not meant to be. What
 * this writes is a service, a volume and a password, all of which the platform
 * already has, plus the one thing that is genuinely hard to get right: the
 * durability mode, and a record of which one is in use.
 *
 * The DEFAULT is the architecture's default (`ARCHITECTURE.md:677-690`): a
 * local data directory with WAL shipped to a volume every 60 seconds. Per-write
 * durability on a volume means an object-storage round trip per commit, which
 * a Postgres data directory pays on every transaction. So the trade is RPO for
 * commit latency, and neither is strictly better. `--durable-volume` takes the
 * other side.
 *
 * The WAL archive target is a pilots VOLUME rather than S3 directly, because
 * the stock `postgres:17` image carries no S3 tooling and pilots has no bind
 * mounts. A volume is JuiceFS over S3, so a `cp` into `/archive` lands in
 * object storage with nothing extra in the image.
 */

import { randomBytes } from 'node:crypto'

export type PostgresMode = 'wal-archive' | 'durable-volume'

export interface Fragment {
  /** The service block, as a plain object the YAML writer serialises. */
  service: Record<string, unknown>
  /** Top-level volumes the fragment needs. */
  volumes: Record<string, Record<string, never>>
  /** Generated files, relative to the compose file's directory. */
  files: Record<string, string>
  mode: PostgresMode
  /** The trailing comment on the `x-pilots.durable_volume` line. */
  modeComment: string
}

/**
 * 32 base64url characters from 24 random bytes.
 *
 * base64url rather than base64 or hex: the value goes into a `postgres://` URL
 * and into a shell environment, and `+`, `/` and `=` need escaping in both.
 */
export function generatePassword(): string {
  return randomBytes(24).toString('base64url')
}

export function databaseURL(password: string, service = 'postgres'): string {
  // `postgres.internal` is the service name as #26's resolver serves it. There
  // is no app prefix in an `.internal` name.
  return `postgres://postgres:${password}@${service}.internal:5432/postgres`
}

export function postgresFragment(opts: { durableVolume?: boolean } = {}): Fragment {
  if (opts.durableVolume) {
    return {
      mode: 'durable-volume',
      modeComment: ' data dir on a volume: RPO 0, at an object-storage round trip per commit',
      service: {
        image: 'postgres:17',
        environment: {
          POSTGRES_PASSWORD: 'secret://postgres_password',
          PGDATA: '/var/lib/postgresql/data',
        },
        volumes: ['pgdata:/var/lib/postgresql/data'],
        healthcheck: {
          test: ['CMD-SHELL', 'pg_isready -U postgres'],
          interval: '10s',
          timeout: '5s',
          retries: 5,
        },
        'x-pilots': { durable_volume: true },
      },
      volumes: { pgdata: {} },
      files: {},
    }
  }

  return {
    mode: 'wal-archive',
    modeComment: ' data dir local, WAL shipped to the pgarchive volume every 60s (RPO 60s)',
    service: {
      build: './.pilots/postgres',
      // A list rather than a string: the archive_command contains spaces and
      // `&&`, and a string would be re-split by whatever runs it.
      command: [
        'postgres',
        '-c',
        'archive_mode=on',
        '-c',
        'archive_timeout=60',
        '-c',
        'archive_command=test ! -f /archive/wal/%f && cp %p /archive/wal/%f',
      ],
      environment: {
        POSTGRES_PASSWORD: 'secret://postgres_password',
        PGDATA: '/var/lib/postgresql/data',
      },
      volumes: ['pgarchive:/archive'],
      healthcheck: {
        test: ['CMD-SHELL', 'pg_isready -U postgres'],
        interval: '10s',
        timeout: '5s',
        retries: 5,
      },
      'x-pilots': { durable_volume: false },
    },
    volumes: { pgarchive: {} },
    files: {
      '.pilots/postgres/Dockerfile': DOCKERFILE,
      '.pilots/postgres/10-base-backup.sh': BASE_BACKUP,
    },
  }
}

/**
 * Two lines on top of the stock image.
 *
 * The base backup has to be taken by an init script rather than afterwards:
 * the entrypoint starts its temporary server with the same `command`
 * arguments, so archiving is already on when the first WAL segment is written,
 * and a backup taken here is consistent with the archive that follows it.
 */
const DOCKERFILE = `FROM postgres:17
COPY 10-base-backup.sh /docker-entrypoint-initdb.d/
`

const BASE_BACKUP = `#!/bin/sh
# Taken once, during initdb, into the archive volume. Point-in-time recovery is
# this backup plus every WAL segment archived after it; see docs/postgres.md.
set -e
mkdir -p /archive/wal /archive/base
pg_basebackup -U postgres -D /archive/base -Ft -z -X none
`
