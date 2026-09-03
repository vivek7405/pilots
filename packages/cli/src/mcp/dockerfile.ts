/**
 * `generate_dockerfile`: the recipe half of prompt-to-URL deploy.
 *
 * An agent pointed at a repo with no Dockerfile has to write one, and there
 * are exactly two ways to get it wrong that produce a build which SUCCEEDS and
 * a URL that answers 502:
 *
 *   1. binding `127.0.0.1` instead of `0.0.0.0`, so the guest serves only
 *      itself and the router's proxy into the netns connects to nothing;
 *   2. ignoring `$PORT`, so the app listens somewhere the router is not
 *      looking.
 *
 * Every recipe below does both, and the test asserts both for every row. A
 * `127.0.0.1` is allowed on a HEALTHCHECK line and nowhere else: the probe runs
 * INSIDE the guest, where the loopback is the right target.
 */

import { existsSync, readdirSync, readFileSync, statSync } from 'node:fs'
import { join } from 'node:path'

export type Framework =
  | 'webjs'
  | 'next'
  | 'react-router'
  | 'vite'
  | 'django'
  | 'fastapi'
  | 'rails'
  | 'go'
  | 'rust'
  | 'laravel'
  | 'unknown'

export interface Health {
  type: 'http'
  path: string
  grace?: number
}

export interface Recipe {
  framework: Framework
  dockerfile: string
  port: number
  health: Health
  notes: string[]
}

/** Every file the detector looks at, for the `unknown` message. */
const LOOKED_FOR = [
  'package.json (with a @webjsdev/* dependency)',
  'next.config.{js,ts,mjs,cjs}',
  'react-router.config.* or remix.config.*',
  'vite.config.*',
  'manage.py with requirements.txt or pyproject.toml',
  'main.py or app.py importing fastapi',
  'Gemfile with bin/rails',
  'go.mod',
  'Cargo.toml',
  'composer.json with artisan',
]

const LOCKFILES = ['package-lock.json', 'npm-shrinkwrap.json', 'yarn.lock', 'pnpm-lock.yaml', 'bun.lockb']

export function detectFramework(dir: string): Framework {
  const has = (name: string) => existsSync(join(dir, name))
  const hasPrefix = (prefix: string) => listing(dir).some((f) => f.startsWith(prefix))
  const pkg = readJSON(join(dir, 'package.json'))

  // webjs first, and by dependency rather than by config file: a webjs app has
  // no build step and no bundler config to detect, which is the whole point of
  // the framework.
  if (pkg && dependencyNames(pkg).some((name) => name.startsWith('@webjsdev/'))) return 'webjs'
  if (hasPrefix('next.config.') && LOCKFILES.some(has)) return 'next'
  if (hasPrefix('react-router.config.') || hasPrefix('remix.config.')) return 'react-router'
  if (hasPrefix('vite.config.')) return 'vite'
  if (has('manage.py') && (has('requirements.txt') || has('pyproject.toml'))) return 'django'
  if (importsFastAPI(dir)) return 'fastapi'
  if (has('Gemfile') && has(join('bin', 'rails'))) return 'rails'
  if (has('go.mod')) return 'go'
  if (has('Cargo.toml')) return 'rust'
  if (has('composer.json') && has('artisan')) return 'laravel'
  return 'unknown'
}

export function generateDockerfile(dir: string): Recipe {
  const framework = detectFramework(dir)
  switch (framework) {
    case 'webjs':
      return webjs()
    case 'next':
      return next()
    case 'react-router':
      return reactRouter()
    case 'vite':
      return vite()
    case 'django':
      return django(dir)
    case 'fastapi':
      return fastapi(dir)
    case 'rails':
      return rails()
    case 'go':
      return go()
    case 'rust':
      return rust(dir)
    case 'laravel':
      return laravel()
    default:
      return {
        framework: 'unknown',
        dockerfile: '',
        port: 0,
        health: { type: 'http', path: '/' },
        notes: [
          'no framework was detected. Looked for, in order:',
          ...LOOKED_FOR.map((f) => `  ${f}`),
          'Write a Dockerfile by hand and pass it to `build`. Two rules: bind 0.0.0.0, never 127.0.0.1, and read the port from $PORT.',
        ],
      }
  }
}

// ---------------------------------------------------------------------------
// The recipes. Every one binds 0.0.0.0 and honours $PORT.
// ---------------------------------------------------------------------------

/**
 * The scaffold Dockerfile, minus its comments.
 *
 * The HEALTHCHECK's `127.0.0.1` is the probe's target inside the guest, not a
 * bind address, and it maps onto the tagged union hostd's health field is.
 */
function webjs(): Recipe {
  return {
    framework: 'webjs',
    port: 8080,
    health: { type: 'http', path: '/__webjs/ready', grace: 40 },
    notes: [
      'webjs is buildless: there is no bundler step, and `npm start` serves .ts directly.',
      '`webjs start` reads $PORT and listens on 0.0.0.0 itself, which is why the Dockerfile only sets the default.',
      '/__webjs/ready answers 503 until the instance is warm, then 200, which is what the grace period is for.',
    ],
    dockerfile: `FROM node:24-alpine
RUN apk add --no-cache ca-certificates
WORKDIR /app
COPY package.json package-lock.json* ./
RUN npm install --no-audit --no-fund
COPY . .
ENV NODE_ENV=production
ENV PORT=8080
EXPOSE 8080
HEALTHCHECK --interval=15s --timeout=3s --start-period=40s --retries=5 \\
  CMD ["node", "-e", "fetch('http://127.0.0.1:'+(process.env.PORT||8080)+'/__webjs/ready').then(r=>process.exit(r.ok?0:1),()=>process.exit(1))"]
CMD ["npm", "start"]
`,
  }
}

function next(): Recipe {
  return {
    framework: 'next',
    port: 3000,
    health: { type: 'http', path: '/' },
    notes: [
      'Next binds 127.0.0.1 by default, which serves nothing outside the guest; -H 0.0.0.0 is not optional here.',
    ],
    dockerfile: `FROM node:24-alpine
RUN apk add --no-cache ca-certificates
WORKDIR /app
COPY package.json package-lock.json* ./
RUN npm ci
COPY . .
RUN npm run build
ENV NODE_ENV=production
ENV PORT=3000
EXPOSE 3000
CMD ["sh", "-c", "npm start -- -H 0.0.0.0 -p \${PORT:-3000}"]
`,
  }
}

function reactRouter(): Recipe {
  return {
    framework: 'react-router',
    port: 3000,
    health: { type: 'http', path: '/' },
    notes: ['The server reads HOST and PORT from the environment; both are set explicitly here.'],
    dockerfile: `FROM node:24-alpine
RUN apk add --no-cache ca-certificates
WORKDIR /app
COPY package.json package-lock.json* ./
RUN npm ci
COPY . .
RUN npm run build
ENV NODE_ENV=production
ENV HOST=0.0.0.0
ENV PORT=3000
EXPOSE 3000
CMD ["sh", "-c", "HOST=0.0.0.0 PORT=\${PORT:-3000} npm start"]
`,
  }
}

/**
 * A static build behind nginx.
 *
 * nginx cannot read an environment variable in its config, so the listen
 * directive is templated with `envsubst` at start. Hard-coding 80 would work
 * until the fleet handed the machine a different port.
 */
function vite(): Recipe {
  return {
    framework: 'vite',
    port: 80,
    health: { type: 'http', path: '/' },
    notes: [
      'A static bundle: there is no application server, so nginx serves the build output.',
      'nginx has no environment interpolation, so the listen directive is templated with envsubst at start.',
    ],
    dockerfile: `FROM node:24-alpine AS build
WORKDIR /app
COPY package.json package-lock.json* ./
RUN npm ci
COPY . .
RUN npm run build

FROM nginx:alpine
RUN rm /etc/nginx/conf.d/default.conf
COPY --from=build /app/dist /usr/share/nginx/html
RUN printf '%s\\n' \\
  'server {' \\
  '  listen \${PORT};' \\
  '  root /usr/share/nginx/html;' \\
  '  location / { try_files $uri $uri/ /index.html; }' \\
  '}' > /etc/nginx/templates/default.conf.template
ENV PORT=80
EXPOSE 80
CMD ["sh", "-c", "envsubst '$PORT' < /etc/nginx/templates/default.conf.template > /etc/nginx/conf.d/default.conf && nginx -g 'daemon off;'"]
`,
  }
}

function django(dir: string): Recipe {
  const project = findWSGIProject(dir) ?? 'app'
  const collectstatic = hasStaticRoot(dir, project)
  const start =
    'python manage.py migrate --noinput' +
    (collectstatic ? ' && python manage.py collectstatic --noinput' : '') +
    ` && gunicorn ${project}.wsgi:application --bind 0.0.0.0:\${PORT:-8000}`
  return {
    framework: 'django',
    port: 8000,
    health: { type: 'http', path: '/', grace: 30 },
    notes: [
      "ALLOWED_HOSTS must include the machine's hostname (['*'] or os.environ['DJANGO_ALLOWED_HOSTS'].split(',')), or every request is a 400 DisallowedHost.",
      collectstatic
        ? 'STATIC_ROOT is set, so collectstatic runs at start.'
        : 'STATIC_ROOT is not set, so collectstatic is omitted: on a bare project it would fail the start.',
      'The migration runs at start here. For a service with replicas, move it to x-pilots.pre_deploy so it runs once rather than once per replica.',
    ],
    dockerfile: `FROM python:3.12-slim
ENV PYTHONDONTWRITEBYTECODE=1 PYTHONUNBUFFERED=1 PORT=8000
WORKDIR /app
COPY requirements.txt ./
RUN pip install --no-cache-dir -r requirements.txt gunicorn
COPY . .
EXPOSE 8000
CMD ["sh", "-c", "${start}"]
`,
  }
}

function fastapi(dir: string): Recipe {
  const module = existsSync(join(dir, 'main.py')) ? 'main' : 'app'
  return {
    framework: 'fastapi',
    port: 8000,
    health: { type: 'http', path: '/' },
    notes: [`The ASGI app is taken as ${module}:app; rename the target if the callable is not called \`app\`.`],
    dockerfile: `FROM python:3.12-slim
ENV PYTHONDONTWRITEBYTECODE=1 PYTHONUNBUFFERED=1 PORT=8000
WORKDIR /app
COPY requirements.txt ./
RUN pip install --no-cache-dir -r requirements.txt uvicorn
COPY . .
EXPOSE 8000
CMD ["sh", "-c", "uvicorn ${module}:app --host 0.0.0.0 --port \${PORT:-8000}"]
`,
  }
}

function rails(): Recipe {
  return {
    framework: 'rails',
    port: 3000,
    health: { type: 'http', path: '/up' },
    notes: [
      'Rails 7.1 and newer serve /up as a health endpoint, which is what the check polls.',
      'SECRET_KEY_BASE has to be set for a production boot; pass it as a sealed environment variable.',
    ],
    dockerfile: `FROM ruby:3.3-slim
RUN apt-get update && apt-get install -y --no-install-recommends build-essential libpq-dev ca-certificates && rm -rf /var/lib/apt/lists/*
ENV RAILS_ENV=production RAILS_LOG_TO_STDOUT=1 PORT=3000
WORKDIR /app
COPY Gemfile Gemfile.lock ./
RUN bundle install
COPY . .
RUN SECRET_KEY_BASE=build-only bundle exec rake assets:precompile
EXPOSE 3000
CMD ["sh", "-c", "bundle exec puma -b tcp://0.0.0.0:\${PORT:-3000}"]
`,
  }
}

function go(): Recipe {
  return {
    framework: 'go',
    port: 8080,
    health: { type: 'http', path: '/' },
    notes: [
      'The binary must read PORT from the environment and listen on 0.0.0.0 (or ":"+port, which binds every interface). A hard-coded 127.0.0.1 builds cleanly and answers 502.',
      'The runtime stage is distroless, so there is no shell in the image: `exec` into this machine will not find /bin/sh.',
    ],
    dockerfile: `FROM golang:1.23 AS build
WORKDIR /src
COPY go.mod go.sum* ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /app ./...

FROM gcr.io/distroless/static
COPY --from=build /app /app
ENV PORT=8080
EXPOSE 8080
CMD ["/app"]
`,
  }
}

function rust(dir: string): Recipe {
  const name = cargoPackageName(dir) ?? 'app'
  return {
    framework: 'rust',
    port: 8080,
    health: { type: 'http', path: '/' },
    notes: [
      'The binary must read PORT from the environment and bind 0.0.0.0. A hard-coded 127.0.0.1 builds cleanly and answers 502.',
      `The release binary is taken as target/release/${name}, from the package name in Cargo.toml.`,
    ],
    dockerfile: `FROM rust:1-slim AS build
WORKDIR /src
COPY . .
RUN cargo build --release

FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates && rm -rf /var/lib/apt/lists/*
COPY --from=build /src/target/release/${name} /app
ENV PORT=8080
EXPOSE 8080
CMD ["/app"]
`,
  }
}

function laravel(): Recipe {
  return {
    framework: 'laravel',
    port: 8000,
    health: { type: 'http', path: '/' },
    notes: [
      'APP_KEY has to be set, or every boot fails; pass it as a sealed environment variable.',
      '`artisan serve` is PHP\'s built-in server and is single-threaded. For real traffic, swap it for php-fpm behind nginx.',
    ],
    dockerfile: `FROM php:8.3-cli
RUN apt-get update && apt-get install -y --no-install-recommends git unzip libpq-dev && docker-php-ext-install pdo_pgsql && rm -rf /var/lib/apt/lists/*
COPY --from=composer:2 /usr/bin/composer /usr/bin/composer
ENV PORT=8000
WORKDIR /app
COPY composer.json composer.lock* ./
RUN composer install --no-dev --no-scripts --no-autoloader
COPY . .
RUN composer dump-autoload --optimize
EXPOSE 8000
CMD ["sh", "-c", "php artisan serve --host=0.0.0.0 --port=\${PORT:-8000}"]
`,
  }
}

// ---------------------------------------------------------------------------
// Detection helpers.
// ---------------------------------------------------------------------------

function listing(dir: string): string[] {
  try {
    return readdirSync(dir)
  } catch {
    return []
  }
}

function readJSON(path: string): Record<string, unknown> | null {
  try {
    return JSON.parse(readFileSync(path, 'utf8')) as Record<string, unknown>
  } catch {
    return null
  }
}

function dependencyNames(pkg: Record<string, unknown>): string[] {
  const out: string[] = []
  for (const field of ['dependencies', 'devDependencies', 'peerDependencies']) {
    const deps = pkg[field]
    if (deps && typeof deps === 'object') out.push(...Object.keys(deps as object))
  }
  return out
}

function importsFastAPI(dir: string): boolean {
  for (const name of ['main.py', 'app.py']) {
    try {
      if (/^\s*(from\s+fastapi|import\s+fastapi)/m.test(readFileSync(join(dir, name), 'utf8'))) return true
    } catch {
      // Not there, or not readable: the next candidate decides.
    }
  }
  return false
}

/** The package directory holding `wsgi.py`, which names the WSGI module. */
export function findWSGIProject(dir: string): string | null {
  for (const name of listing(dir)) {
    try {
      if (!statSync(join(dir, name)).isDirectory()) continue
    } catch {
      continue
    }
    if (existsSync(join(dir, name, 'wsgi.py'))) return name
  }
  return null
}

/**
 * Whether `collectstatic` would have anywhere to write.
 *
 * A bare `django-admin startproject` sets no STATIC_ROOT, and running
 * collectstatic without one fails the start with an ImproperlyConfigured. So
 * the step is added only when the setting is there.
 */
function hasStaticRoot(dir: string, project: string): boolean {
  try {
    return /^\s*STATIC_ROOT\s*=/m.test(readFileSync(join(dir, project, 'settings.py'), 'utf8'))
  } catch {
    return false
  }
}

function cargoPackageName(dir: string): string | null {
  try {
    const text = readFileSync(join(dir, 'Cargo.toml'), 'utf8')
    const match = text.match(/^\s*name\s*=\s*"([^"]+)"/m)
    return match?.[1] ?? null
  } catch {
    return null
  }
}
