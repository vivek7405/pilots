'use server';
/**
 * Deploy a repository from the browser: plan it again, start the build, create
 * the service, record the build, and land on the deployments view that follows
 * the build to its verdict.
 *
 * The form is not trusted with the plan. It is re-planned here, because the
 * step it saw a moment ago may not be the step the host would build now, and
 * because a client must not be able to name replicas, health or env for a
 * service by hand. Only what a person typed is taken from the form: the name,
 * the domain, and a value per secret the plan asked for.
 *
 * No request waits on the build. The router's cross-host forward times out
 * response headers at two minutes, and a build is longer than that, so this
 * starts it, lets the stream go, creates the service, and redirects. hostd
 * keeps building; the deployments view follows the job by id.
 */
import { PilotsError } from '@pilots/sdk';
import type { CreateServiceRequest } from '@pilots/sdk';
import { requireOrg } from '#modules/auth/session.server.ts';
import { fleetAs } from '#modules/fleet/client.server.ts';
import { installationFor } from '#modules/github/installations.server.ts';
import { isRepoSlug } from '#modules/domains/hostname.ts';
import { db } from '#db/connection.server.ts';
import { builds, repoConnections } from '#db/schema.server.ts';

const NAME = /^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$/;

export async function createFromRepo(formData: FormData) {
  const ctx = await requireOrg();
  if (!ctx) return { success: false, error: 'Sign in to continue.', status: 401 };

  const repo = String(formData.get('repo') || '').trim();
  const ref = String(formData.get('ref') || 'main').trim();
  const app = String(formData.get('app') || '').trim() || repo.split('/')[1] || '';
  const name = String(formData.get('name') || '').trim();
  const domain = String(formData.get('domain') || '').trim();
  const fieldErrors: Record<string, string> = {};
  if (!isRepoSlug(repo)) fieldErrors.repo = 'Use owner/name.';
  if (!NAME.test(name)) fieldErrors.name = 'Lowercase letters, digits and hyphens, up to 63.';
  // `app` is checked like its siblings, and it is the one that is easy to
  // miss: it is the only name here that can be DERIVED rather than typed
  // (from the repo half of owner/name), so an unvalidated one reaches hostd
  // as an app group nobody typed and nobody can address.
  if (!NAME.test(app)) fieldErrors.app = 'Lowercase letters, digits and hyphens, up to 63.';
  if (domain && !NAME.test(domain)) fieldErrors.domain = 'Lowercase letters, digits and hyphens, up to 63.';
  if (Object.keys(fieldErrors).length > 0) return { success: false, fieldErrors, status: 422 };

  const client = fleetAs(ctx.org.id);
  try {
    // Re-plan: the form is not trusted with the step.
    const planned = await client.planRepo({ repo, ref }, { app });
    if (planned.plan.steps.length !== 1) {
      return {
        success: false,
        status: 422,
        error: `This repository deploys as ${planned.plan.steps.length} services. Use pilot deploy for a multi-service app.`,
      };
    }
    const step = planned.plan.steps[0];
    if (step.volumes && step.volumes.length > 0) {
      return { success: false, status: 422, error: 'This service needs storage. Use pilot deploy, which can attach it.' };
    }

    // A value per secret the plan asked for, from the password inputs.
    const secretEnv: Record<string, string> = {};
    for (const key of Object.keys(step.secret_refs ?? {})) {
      const value = String(formData.get(`secret:${key}`) || '');
      if (!value) return { success: false, status: 422, fieldErrors: { [`secret:${key}`]: 'Required.' } };
      secretEnv[key] = value;
    }

    const create: CreateServiceRequest = {
      name,
      app,
      replicas: step.replicas,
      repo,
      branch: ref,
      autodeploy: true,
    };
    if (step.health) create.health = step.health;
    if (step.env && Object.keys(step.env).length > 0) create.env = step.env;
    if (Object.keys(secretEnv).length > 0) create.secret_env = secretEnv;
    if (domain) create.domain = domain;
    // The SERVICE first, then the build. The other order started a real build
    // on a host and only then asked hostd to create the service, so a refused
    // create -- a name already taken, a domain already claimed, a quota --
    // left a build running against a MaxBuilds slot with no service to
    // deliver to and no `builds` row to find it by, reported to the person as
    // a generic 502.
    //
    // The failure this order can produce instead is a service with no release
    // yet, which the Deployments tab already has words for and which its
    // owner can retry or remove.
    const service = await client.services.create(create);

    // Start the build and let the stream go: hostd continues without us.
    const stream = await client.builds.createFromRepo({ repo, ref }, { app });
    const jobId = stream.buildId;
    await stream.close();

    await db
      .insert(builds)
      .values({ orgId: ctx.org.id, serviceId: service.id, jobId, repo, ref, startedBy: ctx.user.id })
      .run();
    const installation = await installationFor(repo.split('/')[0]).catch(() => null);
    await db
      .insert(repoConnections)
      .values({
        orgId: ctx.org.id,
        serviceId: service.id,
        repo,
        branch: ref,
        autodeploy: true,
        installationId: installation?.id ?? null,
        connectedBy: ctx.user.id,
      })
      .onConflictDoUpdate({
        target: repoConnections.serviceId,
        set: { repo, branch: ref, autodeploy: true, installationId: installation?.id ?? null, updatedAt: new Date() },
      })
      .run();

    return {
      success: true,
      redirect: `/services/${service.id}?tab=deployments&build=${encodeURIComponent(jobId)}&ok=building`,
    };
  } catch (err) {
    if (err instanceof PilotsError) {
      // The engine's own verdict when it gave one. A planner refusal (400), a
      // taken name (409), a gate (422) or a quota (429) is the caller's to fix;
      // only a status the engine did not choose -- a dropped connection, a 5xx
      // -- is the fleet being unavailable, and says so as 502.
      const status = err.status >= 400 && err.status < 500 ? err.status : 502;
      return { success: false, status, error: err.next ? `${err.message} ${err.next}` : err.message };
    }
    return { success: false, status: 502, error: (err as Error).message };
  }
}
