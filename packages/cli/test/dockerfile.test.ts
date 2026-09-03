/**
 * Framework detection and the recipes it picks.
 *
 * The two assertions that run over EVERY row are the point of the file: bind
 * 0.0.0.0, and read $PORT. Both failures produce a build that succeeds and a
 * URL that answers 502, which is the single most expensive way for an agent's
 * deploy loop to go wrong, because there is nothing in the build log to read.
 */

import { strict as assert } from 'node:assert'
import { mkdtempSync, rmSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { after, test } from 'node:test'

import { detectFramework, generateDockerfile, type Framework } from '../src/mcp/dockerfile.ts'

const FIXTURES = join(import.meta.dirname, 'fixtures', 'frameworks')
const roots: string[] = []
after(() => {
  for (const dir of roots) rmSync(dir, { recursive: true, force: true })
})

interface Row {
  fixture: string
  framework: Framework
  base: string
  port: number
  healthPath: string
}

const ROWS: Row[] = [
  { fixture: 'webjs', framework: 'webjs', base: 'FROM node:24-alpine', port: 8080, healthPath: '/__webjs/ready' },
  { fixture: 'next', framework: 'next', base: 'FROM node:24-alpine', port: 3000, healthPath: '/' },
  { fixture: 'react-router', framework: 'react-router', base: 'FROM node:24-alpine', port: 3000, healthPath: '/' },
  { fixture: 'vite', framework: 'vite', base: 'FROM nginx:alpine', port: 80, healthPath: '/' },
  { fixture: 'django', framework: 'django', base: 'FROM python:3.12-slim', port: 8000, healthPath: '/' },
  { fixture: 'fastapi', framework: 'fastapi', base: 'FROM python:3.12-slim', port: 8000, healthPath: '/' },
  { fixture: 'rails', framework: 'rails', base: 'FROM ruby:3.3-slim', port: 3000, healthPath: '/up' },
  { fixture: 'go', framework: 'go', base: 'FROM gcr.io/distroless/static', port: 8080, healthPath: '/' },
  { fixture: 'rust', framework: 'rust', base: 'FROM debian:bookworm-slim', port: 8080, healthPath: '/' },
  { fixture: 'laravel', framework: 'laravel', base: 'FROM php:8.3-cli', port: 8000, healthPath: '/' },
]

for (const row of ROWS) {
  test(`${row.fixture} is detected and gets its own recipe`, () => {
    const dir = join(FIXTURES, row.fixture)
    assert.equal(detectFramework(dir), row.framework)
    const recipe = generateDockerfile(dir)
    assert.equal(recipe.framework, row.framework)
    assert.equal(recipe.port, row.port)
    assert.equal(recipe.health.path, row.healthPath)
    assert.ok(recipe.dockerfile.includes(row.base), `${row.fixture} does not use ${row.base}`)
    assert.ok(recipe.notes.length > 0, 'every recipe says something the agent needs to know')
  })
}

test('every recipe declares the port it serves, as PORT', () => {
  for (const row of ROWS) {
    const recipe = generateDockerfile(join(FIXTURES, row.fixture))
    // An ENV line carrying PORT=<the row's port>. Without it the app falls
    // back to whatever its framework defaults to, which is not necessarily
    // the port the health check polls.
    const declared = recipe.dockerfile
      .split('\n')
      .filter((line) => line.startsWith('ENV '))
      .map((line) => line.match(/(?:^|\s)PORT=(\d+)/)?.[1])
      .find((value) => value !== undefined)
    assert.equal(declared, String(row.port), `${row.fixture} does not declare PORT=${row.port}`)
  }
})

test('a recipe whose start command names a port interpolates it', () => {
  for (const row of ROWS) {
    const recipe = generateDockerfile(join(FIXTURES, row.fixture))
    const start = recipe.dockerfile.split('\n').filter((line) => line.startsWith('CMD ')).join('\n')
    if (!/\d{2,5}/.test(start)) continue
    // A hard-coded 3000 in the start command is a service listening where the
    // router is not looking, and nothing in the build log says so.
    assert.match(start, /\$\{PORT/, `${row.fixture} hard-codes a port in its CMD`)
  }
})

test('every recipe binds all interfaces rather than loopback', () => {
  for (const row of ROWS) {
    const recipe = generateDockerfile(join(FIXTURES, row.fixture))
    const binds = /0\.0\.0\.0|HOST=0\.0\.0\.0|listen \$\{PORT\}/.test(recipe.dockerfile)
    const appBinds = ['webjs', 'go', 'rust'].includes(row.fixture)
    if (appBinds) {
      // These three bind inside the application rather than in the Dockerfile,
      // so the recipe cannot enforce it; the notes have to say so instead.
      assert.ok(
        recipe.notes.some((n) => n.includes('0.0.0.0') || n.includes('all interfaces')),
        `${row.fixture} neither binds nor warns about binding`,
      )
      continue
    }
    assert.ok(binds, `${row.fixture} does not bind 0.0.0.0`)
  }
})

test('127.0.0.1 appears only on a HEALTHCHECK line', () => {
  for (const row of ROWS) {
    const recipe = generateDockerfile(join(FIXTURES, row.fixture))
    const offenders = recipe.dockerfile
      .split('\n')
      .filter((line) => line.includes('127.0.0.1'))
      .filter((line) => !/HEALTHCHECK|CMD \["node", "-e"/.test(line))
    // The probe runs inside the guest, where loopback is right. Anywhere else
    // it is a service that answers only itself.
    assert.deepEqual(offenders, [], `${row.fixture} binds loopback outside a health probe`)
  }
})

test('the webjs recipe is the scaffold Dockerfile with the comments stripped', () => {
  const recipe = generateDockerfile(join(FIXTURES, 'webjs'))
  assert.equal(
    recipe.dockerfile,
    `FROM node:24-alpine
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
  )
  assert.equal(recipe.health.grace, 40, 'the readiness gate answers 503 until warm')
})

test('the Django recipe names the project holding wsgi.py', () => {
  const recipe = generateDockerfile(join(FIXTURES, 'django'))
  assert.match(recipe.dockerfile, /gunicorn mysite\.wsgi:application --bind 0\.0\.0\.0:\$\{PORT:-8000\}/)
  assert.match(recipe.dockerfile, /python manage\.py migrate --noinput/)
  // The one thing a bare startproject gets wrong on any host.
  assert.ok(recipe.notes.some((n) => n.includes('ALLOWED_HOSTS') && n.includes('DisallowedHost')))
})

test('collectstatic is added only when STATIC_ROOT is set', () => {
  const bare = generateDockerfile(join(FIXTURES, 'django'))
  assert.equal(bare.dockerfile.includes('collectstatic'), false, 'a bare project has no STATIC_ROOT to collect into')

  const withRoot = generateDockerfile(join(FIXTURES, 'django-static'))
  assert.match(withRoot.dockerfile, /collectstatic --noinput/)
  assert.match(withRoot.dockerfile, /gunicorn shopsite\.wsgi:application/)
})

test('the Rust recipe copies the binary the package name declares', () => {
  assert.match(generateDockerfile(join(FIXTURES, 'rust')).dockerfile, /target\/release\/shopserver \/app/)
})

test('a webjs app wins over any other signal in its package.json', () => {
  const dir = mkdtempSync(join(tmpdir(), 'pilot-fw-'))
  roots.push(dir)
  writeFileSync(join(dir, 'package.json'), '{"dependencies":{"@webjsdev/webjs":"1","vite":"6"}}')
  writeFileSync(join(dir, 'vite.config.ts'), 'export default {}')
  // webjs is buildless and has no config file of its own, so it has to be
  // detected first or a stray bundler config takes the app somewhere else.
  assert.equal(detectFramework(dir), 'webjs')
})

test('an empty directory is unknown and lists everything that was looked for', () => {
  const dir = mkdtempSync(join(tmpdir(), 'pilot-fw-empty-'))
  roots.push(dir)
  const recipe = generateDockerfile(dir)
  assert.equal(recipe.framework, 'unknown')
  assert.equal(recipe.dockerfile, '')
  const text = recipe.notes.join('\n')
  for (const marker of ['manage.py', 'go.mod', 'Cargo.toml', 'vite.config', 'composer.json', 'Gemfile']) {
    assert.ok(text.includes(marker), `${marker} is not in the looked-for list`)
  }
  assert.ok(text.includes('0.0.0.0'), 'the two rules are stated even with no recipe')
})

test('a README alone is not a framework', () => {
  assert.equal(detectFramework(join(FIXTURES, 'unknown')), 'unknown')
})

test('next needs a lockfile as well as a config', () => {
  const dir = mkdtempSync(join(tmpdir(), 'pilot-fw-next-'))
  roots.push(dir)
  writeFileSync(join(dir, 'next.config.js'), 'module.exports = {}')
  writeFileSync(join(dir, 'package.json'), '{}')
  assert.equal(detectFramework(dir), 'unknown', 'npm ci without a lockfile fails the build')
})
