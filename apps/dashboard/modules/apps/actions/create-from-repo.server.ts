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

/**
 * A repository's own half of `owner/name`, as an app name this app can address.
 *
 * `acme/My.Repo` and `acme/foo_bar` are perfectly ordinary repositories whose
 * names are not valid here: uppercase, dots and underscores all fail `NAME`.
 * Derived, the failure reached the person as `fieldErrors.app` on a form that
 * carries `app` as a HIDDEN input and never renders that error -- a Deploy
 * button that refused with nothing on screen. A name nobody typed is ours to
 * make addressable rather than to refuse.
 *
 * A typed `app` is never touched: that one the person can see and correct.
 */
function slugifyApp(repoName: string): string {
  return repoName
    .toLowerCase()
    .replace(/[^a-z0-9-]+/g, '-')
    .replace(/^-+|-+$/g, '')
    .slice(0, 63)
    .replace(/-+$/g, '');
}

export async function createFromRepo(formData: FormData) {
  const ctx = await requireOrg();
  if (!ctx) return { success: false, error: 'Sign in to continue.', status: 401 };

  const repo = String(formData.get('repo') || '').trim();
  const ref = String(formData.get('ref') || 'main').trim();
  const typedApp = String(formData.get('app') || '').trim();
  const app = typedApp || slugifyApp(repo.split('/')[1] || '');
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

    // Record the connection ON THE FLEET, before anything is created from it.
    //
    // The row below in this app's own database is what the service page
    // renders; it is not, and cannot be, an authorization record: hostd cannot
    // read this database, and the data plane may not depend on this app
    // (ARCHITECTURE.md invariant 2). hostd keeps its own `repo_links` row and
    // reads it from its local replica on every `{repo, ref}` build.
    //
    // Without this call the deploy would still work -- this app holds an admin
    // key, which may name any repository -- and the org's OWN key would then
    // be refused the repository it just deployed from, by `pilot deploy`, by
    // an agent, by anything that is not this process. First, and not after the
    // service exists, so a fleet that refuses the connection refuses before
    // there is anything to clean up.
    await client.repos.connect(repo);

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
