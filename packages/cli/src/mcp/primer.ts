/**
 * The `init` primer.
 *
 * Under 60 lines, and that is a budget rather than an aspiration: it is the
 * first thing a small model reads, it competes for the same context as the
 * task, and a primer nobody finishes is worse than none. What survived the
 * cut is the one call, what every result and error carries, the primitive,
 * four rules, and the doc index. Everything else is a reference page.
 *
 * A test holds it to the budget, because the natural drift is upward.
 */
export const PRIMER = `pilots: sandboxes and services on one primitive.

THE ONE CALL
  deploy { "dir": "<absolute path>" }
  -> { app, services: [{ name, url, release_id }], next }
  The host decides what the directory is: a compose file, a Dockerfile,
  a recipe (webjs, next, react-router, vite, django, fastapi, rails, go,
  rust, laravel), or unknown. Do not write a Dockerfile first.

EVERY RESULT CARRIES next. EVERY ERROR CARRIES code, next, details.
  Read next. Do that. Nothing else needs planning.

THE ANSWERS YOU WILL SEE
  unknown_framework   read details.listing and details.manifests, write a
                      Dockerfile that obeys details.rules, call build with
                      it, then deploy with name and build.
  build_failed        every log line is in the error; fix the line marked
                      error, call build again.
  health_gate_failed  call diagnose with details.replica; it is almost
                      always the port (read $PORT, 8080) or the bind
                      address (0.0.0.0, never 127.0.0.1).
  plan_unsupported    fix each key in details.unsupported.
  plan_multi_service  commit a compose file; a push deploys one service.
  quota_exceeded      next names the limit.
  not_found           check the id; the key may see a different org.

THE PRIMITIVE
  A machine is a Firecracker microVM. A sandbox and a production replica
  are the same machine with different lifecycle knobs. A service is one
  or more machines behind a permanent URL that survives every deploy.
  create_machine + exec is a sandbox. deploy is a service. promote turns
  the first into the second without changing its URL.

RULES
  No directory and no repo in the conversation: ask, never invent one.
  destroy_machine and rollback change what is live: confirm first.
  After a mutation, read it back with service or status.
  Secrets are secret:// references in a compose file; never paste values.

DOCS
  docs { "topic": "deploy" | "sandboxes" | "services" | "secrets" |
         "volumes" | "domains" | "promote" | "errors" | "compose" }
  Load one. Two at most.
`
