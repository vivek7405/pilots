/**
 * One service: read it, or patch the knobs a human can turn.
 *
 * `env` and `secret_env` REPLACE the stored map rather than merging into it,
 * and take effect at the next deploy. Neither is ever logged.
 */
import { rateLimit } from '@webjsdev/server';
import type { RouteHandlerContext } from '@webjsdev/core';
import type { UpdateServiceRequest } from '@pilots/sdk';
import { orgOr401, isResponse, jsonBody, notFoundResponse, invalidResponse, readJson, str } from '#modules/http/guards.server.ts';
import { fleet } from '#modules/fleet/client.server.ts';
import { assertOwned, fleetErrorResponse } from '#modules/fleet/org-filter.server.ts';
import { isRepoSlug } from '#modules/domains/hostname.ts';

const limited = rateLimit({ window: '1m', max: 30, trustProxy: true, key: 'services:' });

export async function GET(req: Request, { params }: RouteHandlerContext): Promise<Response> {
  const ctx = await orgOr401(req);
  if (isResponse(ctx)) return ctx;
  try {
    const service = assertOwned(ctx.org.id, await fleet.services.get(params.id));
    return service ? jsonBody(service) : notFoundResponse('service');
  } catch (err) {
    return fleetErrorResponse(err);
  }
}

export async function PATCH(req: Request, { params }: RouteHandlerContext): Promise<Response> {
  return limited(req, async () => {
    const ctx = await orgOr401(req);
    if (isResponse(ctx)) return ctx;

    const raw = await readJson(req);
    const fieldErrors: Record<string, string> = {};
    const patch: UpdateServiceRequest = {};

    if (raw.replicas !== undefined) {
      const replicas = Number(raw.replicas);
      if (!Number.isInteger(replicas) || replicas < 0 || replicas > 100) {
        fieldErrors.replicas = 'Replicas must be a whole number from 0 to 100';
      } else patch.replicas = replicas;
    }
    if (raw.repo !== undefined) {
      const repo = str(raw, 'repo');
      // An empty string is how a repo is DISCONNECTED, so it is valid input.
      if (repo && !isRepoSlug(repo)) fieldErrors.repo = 'Use owner/name';
      else patch.repo = repo;
    }
    if (raw.branch !== undefined) patch.branch = str(raw, 'branch').slice(0, 255);
    if (raw.autodeploy !== undefined) patch.autodeploy = Boolean(raw.autodeploy);
    if (isPlainStringMap(raw.env)) patch.env = raw.env;
    if (isPlainStringMap(raw.secret_env)) patch.secret_env = raw.secret_env;

    if (Object.keys(fieldErrors).length) return invalidResponse(fieldErrors);
    if (Object.keys(patch).length === 0) return invalidResponse({ _: 'Nothing to change' });

    try {
      if (!assertOwned(ctx.org.id, await fleet.services.get(params.id))) return notFoundResponse('service');
      return jsonBody(await fleet.services.patch(params.id, patch));
    } catch (err) {
      return fleetErrorResponse(err);
    }
  });
}

function isPlainStringMap(value: unknown): value is Record<string, string> {
  return (
    typeof value === 'object' &&
    value !== null &&
    !Array.isArray(value) &&
    Object.values(value).every((v) => typeof v === 'string')
  );
}
