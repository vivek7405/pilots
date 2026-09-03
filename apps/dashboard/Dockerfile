# Production image for the dashboard.
#
# The build context is the REPO ROOT, not this directory. The dashboard imports
# `@pilots/sdk` from the `sdks/js` workspace, which is not published, so an
# `npm install` run inside a context of `apps/dashboard` could never resolve
# it. Installing from the root with npm workspaces is the only shape that does.
#
# webjs serves `.ts` directly by stripping types at the runtime layer, so there
# is NO JavaScript build step for the app itself (webjs is buildless end to
# end). The SDK is the one thing compiled here, because its package `exports`
# point at `dist/`.
#
# Do not lower the Node base below 24: the built-in TypeScript stripper and
# recursive fs.watch both need it, and it is the floor the framework pins.
#
# Security headers come from the framework, not from a proxy config. webjs emits
# X-Content-Type-Options, X-Frame-Options, Referrer-Policy and
# Permissions-Policy on every response, plus Strict-Transport-Security in
# production over HTTPS, detected from X-Forwarded-Proto on the trusted edge.
FROM node:24-alpine

# ca-certificates for outbound TLS: the fleet API, GitHub, and the per-host
# usage reads. SQLite is the built-in node:sqlite, so there is no native module
# and no build toolchain.
RUN apk add --no-cache ca-certificates

WORKDIR /app

# Manifests first, so this layer caches unless a dependency changes. Every
# workspace the root lockfile names has to be present for `npm ci` to resolve.
COPY package.json package-lock.json ./
COPY apps/dashboard/package.json ./apps/dashboard/
COPY apps/website/package.json ./apps/website/
COPY sdks/js/package.json ./sdks/js/
RUN npm install --no-audit --no-fund --workspace=apps/dashboard --workspace=sdks/js --include-workspace-root

# The SDK's sources, then its build: `@pilots/sdk` resolves through `exports`
# to `dist/`, which is gitignored and therefore never in the context.
COPY sdks/js ./sdks/js
RUN npm run build --workspace=sdks/js

# The app itself. node_modules and local state are excluded by .dockerignore.
COPY apps/dashboard ./apps/dashboard

WORKDIR /app/apps/dashboard

ENV NODE_ENV=production
ENV PORT=8080
EXPOSE 8080

# `/__webjs/ready` answers a real 503 until the instance is fully warm, so this
# is a readiness gate rather than a liveness ping. The rollout cuts over on a
# 200 from the same path.
HEALTHCHECK --interval=15s --timeout=3s --start-period=40s --retries=5 \
  CMD ["node", "-e", "fetch('http://127.0.0.1:'+(process.env.PORT||8080)+'/__webjs/ready').then(r=>process.exit(r.ok?0:1),()=>process.exit(1))"]

# `webjs start` runs its `webjs.start.before` steps in-process (`webjs db
# migrate`, idempotent with no pending migrations) and then serves on $PORT.
CMD ["npm", "start"]
