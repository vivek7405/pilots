package detect

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/vivek7405/pilots/hostd/internal/api"
)

// Recipe is the Dockerfile the platform would write for a directory, with the
// port and the health check that go with it.
type Recipe struct {
	Framework  Framework
	Dockerfile string
	Port       int
	Health     *api.HealthCheck
	Notes      []string
}

// Generate returns the recipe for a directory, and false when no framework was
// recognised. Every recipe declares ENV PORT=8080, EXPOSE 8080 and reads
// ${PORT:-8080} in its start command: the platform's port, never the
// framework's, because the router dials 8080 and an image that listens on 3000
// builds cleanly and answers 502.
func Generate(dir string) (Recipe, bool) { return GenerateIn(dir, dir) }

// GenerateIn is Generate for a directory whose lockfile lives somewhere else.
// See DetectIn: an npm workspace member has no lockfile of its own.
func GenerateIn(dir, lockRoot string) (Recipe, bool) {
	switch f := DetectIn(dir, lockRoot); f {
	case FrameworkWebJS:
		return webjs(), true
	case FrameworkNext:
		return next(), true
	case FrameworkReactRouter:
		return reactRouter(), true
	case FrameworkRemix:
		return remix(dir), true
	case FrameworkVite:
		return vite(), true
	case FrameworkDjango:
		return django(dir), true
	case FrameworkFastAPI:
		return fastapi(dir), true
	case FrameworkRails:
		return rails(), true
	case FrameworkGo:
		return golang(), true
	case FrameworkRust:
		return rust(dir), true
	case FrameworkLaravel:
		return laravel(), true
	default:
		return Recipe{Framework: FrameworkUnknown}, false
	}
}

// workspaceRewritable are the frameworks ForWorkspace knows how to move into a
// subdirectory. They are the node ones, which is all a member of an npm
// workspace can be: the install is npm's, the build script is npm's, and the
// only thing that has to move is the working directory.
//
// Anything else is refused rather than rewritten. A go or rust recipe copies a
// built artifact out of its build stage by absolute path, and moving the
// working directory under it produces a Dockerfile that builds the wrong
// thing or does not build at all. Refusing means the member is skipped and
// named, exactly as an undetected one is, which is a worse outcome than
// working and a much better one than a broken image.
var workspaceRewritable = map[Framework]bool{
	FrameworkWebJS: true, FrameworkNext: true, FrameworkRemix: true,
	FrameworkReactRouter: true, FrameworkVite: true,
}

// ForWorkspace rewrites a recipe for a workspace member of a monorepo, and
// reports whether it could.
//
// The install stays at the root, because that is where the lockfile and the
// hoisted node_modules are, and only the working directory moves. A member
// built from its own directory would reinstall the whole tree per service and
// still miss anything the root hoisted.
func (r Recipe) ForWorkspace(rel string) (Recipe, bool) {
	if !workspaceRewritable[r.Framework] {
		return Recipe{}, false
	}
	out := r
	out.Dockerfile = withWorkdir(r.Dockerfile, rel)
	out.Notes = append(append([]string{}, r.Notes...),
		"Built from the repository root with WORKDIR /app/"+rel+
			": the install is the root's, so the lockfile and the hoisted "+
			"node_modules are the ones the workspace expects.")
	return out, true
}

// withWorkdir moves the build into the member's directory.
//
// The insertion point is the last `COPY . .`, which is the moment the whole
// tree is present and the last moment before anything is built. It is NOT the
// CMD: putting it there left `RUN npm run build` running at the workspace
// root, where an npm-workspaces root usually has no build script at all, so
// the image either failed to build or built the wrong app. For a multi-stage
// recipe it was worse than that, because the CMD is in the runtime stage and
// the WORKDIR landed there instead of in the stage that does the work.
//
// A stage that copies a build artifact out by absolute path has that path
// moved too: vite's `COPY --from=build /app/dist` reads a directory the
// member's build produced under /app/<rel>/dist and nowhere else.
func withWorkdir(dockerfile, rel string) string {
	lines := strings.Split(strings.TrimRight(dockerfile, "\n"), "\n")

	last := -1
	for i, line := range lines {
		if strings.TrimSpace(line) == "COPY . ." {
			last = i
		}
	}
	out := make([]string, 0, len(lines)+1)
	for i, line := range lines {
		// An artifact copied out of an earlier stage moves with the build.
		if strings.HasPrefix(line, "COPY --from=") {
			line = strings.Replace(line, " /app/", " /app/"+rel+"/", 1)
		}
		out = append(out, line)
		if i == last {
			out = append(out, "WORKDIR /app/"+rel)
		}
	}
	if last < 0 {
		// Nothing copied the tree in, so there is no anchor and no build to
		// move; the working directory is all there is to set.
		out = append(out, "WORKDIR /app/"+rel)
	}
	return strings.Join(out, "\n") + "\n"
}

func httpHealth(path string, grace int) *api.HealthCheck {
	h := &api.HealthCheck{Type: "http", Path: path}
	if grace > 0 {
		h.GraceSec = grace
	}
	return h
}

// ---------------------------------------------------------------------------
// The recipes. Every one binds 0.0.0.0 and honours $PORT with 8080.
// ---------------------------------------------------------------------------

// webjs is the scaffold Dockerfile, minus its comments.
//
// The HEALTHCHECK's 127.0.0.1 is the probe's target inside the guest, not a
// bind address, and it maps onto the tagged union hostd's health field is.
func webjs() Recipe {
	return Recipe{
		Framework: FrameworkWebJS,
		Port:      AppPort,
		Health:    httpHealth("/__webjs/ready", 40),
		Notes: []string{
			"webjs is buildless: there is no bundler step, and `npm start` serves .ts directly.",
			"`webjs start` reads $PORT and listens on 0.0.0.0 itself, which is why the Dockerfile only sets the default.",
			"/__webjs/ready answers 503 until the instance is warm, then 200, which is what the grace period is for.",
		},
		Dockerfile: `FROM node:24-alpine
RUN apk add --no-cache ca-certificates
WORKDIR /app
COPY package.json package-lock.json* ./
RUN npm install --no-audit --no-fund
COPY . .
ENV NODE_ENV=production
ENV PORT=8080
EXPOSE 8080
HEALTHCHECK --interval=15s --timeout=3s --start-period=40s --retries=5 \
  CMD ["node", "-e", "fetch('http://127.0.0.1:'+(process.env.PORT||8080)+'/__webjs/ready').then(r=>process.exit(r.ok?0:1),()=>process.exit(1))"]
CMD ["npm", "start"]
`,
	}
}

func next() Recipe {
	return Recipe{
		Framework: FrameworkNext,
		Port:      AppPort,
		Health:    httpHealth("/", 0),
		Notes: []string{
			"Next binds 127.0.0.1 by default, which serves nothing outside the guest; -H 0.0.0.0 is not optional here.",
		},
		Dockerfile: `FROM node:24-alpine
RUN apk add --no-cache ca-certificates
WORKDIR /app
COPY package.json package-lock.json* ./
RUN if [ -f package-lock.json ]; then npm ci; else npm install --no-audit --no-fund; fi
COPY . .
RUN npm run build
ENV NODE_ENV=production
ENV PORT=8080
EXPOSE 8080
CMD ["sh", "-c", "npm start -- -H 0.0.0.0 -p ${PORT:-8080}"]
`,
	}
}

func reactRouter() Recipe {
	return Recipe{
		Framework: FrameworkReactRouter,
		Port:      AppPort,
		Health:    httpHealth("/", 0),
		Notes: []string{
			"The server reads HOST and PORT from the environment; both are set explicitly here.",
		},
		Dockerfile: `FROM node:24-alpine
RUN apk add --no-cache ca-certificates
WORKDIR /app
COPY package.json package-lock.json* ./
RUN if [ -f package-lock.json ]; then npm ci; else npm install --no-audit --no-fund; fi
COPY . .
RUN npm run build
ENV NODE_ENV=production
ENV HOST=0.0.0.0
ENV PORT=8080
EXPOSE 8080
CMD ["sh", "-c", "HOST=0.0.0.0 PORT=${PORT:-8080} npm start"]
`,
	}
}

// vite is a static build behind nginx.
//
// nginx cannot read an environment variable in its config, so the listen
// directive is templated with envsubst at start. Hard-coding a port would work
// until the fleet handed the machine a different one.
// remix is the recipe for Remix 3, the bare `remix` package.
//
// Remix 3 is buildless in the same sense webjs is: there is no bundler step
// and no build output. `npm start` is
// `node --import remix/node-tsx server.ts`, which strips the types at import
// time and serves the source. So the recipe has no RUN npm run build, and
// leaving one in would fail the image on a script that does not exist.
//
// dir is read for remix.json, which is where Remix 3 declares its database.
// A project that declares migrations gets them run at start, the same shape
// the django recipe uses and with the same caveat in the notes.
func remix(dir string) Recipe {
	start := "npm start"
	notes := []string{
		"Remix 3 is buildless: `npm start` runs `node --import remix/node-tsx server.ts`, which serves the TypeScript source directly. There is no build step to run, and no build output for a host to consume.",
		"server.ts is the entry point and owns the listen call, so it must read process.env.PORT and bind 0.0.0.0. A server.ts that hardcodes a port builds cleanly and answers 502.",
		"node:24-alpine is not a default here: the remix package declares engines.node >= 24.3.0, and an older runtime fails at the first import.",
	}
	if remixDeclaresMigrations(dir) {
		start = "npx --no-install remix db migrate && npm start"
		notes = append(notes,
			"remix.json declares migrations, so `remix db migrate` runs at start.",
			"The migration runs at start here. For a service with replicas, move it to x-pilots.pre_deploy so it runs once rather than once per replica.",
			"A sqlite adapter in remix.json writes inside the machine. Attach a volume at the directory holding that file, or the database is lost on the next release.",
		)
	}
	return Recipe{
		Framework: FrameworkRemix,
		Port:      AppPort,
		Health:    httpHealth("/", 0),
		Notes:     notes,
		Dockerfile: `FROM node:24-alpine
RUN apk add --no-cache ca-certificates
WORKDIR /app
COPY package.json package-lock.json* ./
RUN if [ -f package-lock.json ]; then npm ci; else npm install --no-audit --no-fund; fi
COPY . .
ENV NODE_ENV=production
ENV HOST=0.0.0.0
ENV PORT=8080
EXPOSE 8080
CMD ["sh", "-c", "HOST=0.0.0.0 PORT=${PORT:-8080} ` + start + `"]
`,
	}
}

// remixDeclaresMigrations reports whether remix.json names a migrations
// directory. Absent or unreadable answers false: a recipe that runs a
// migration a project has not configured fails the start, which is a worse
// outcome than not running one.
func remixDeclaresMigrations(dir string) bool {
	raw, err := os.ReadFile(filepath.Join(dir, "remix.json"))
	if err != nil {
		return false
	}
	var cfg struct {
		DB struct {
			Migrations struct {
				Directory string `json:"directory"`
			} `json:"migrations"`
		} `json:"db"`
	}
	if json.Unmarshal(raw, &cfg) != nil {
		return false
	}
	return cfg.DB.Migrations.Directory != ""
}

func vite() Recipe {
	return Recipe{
		Framework: FrameworkVite,
		Port:      AppPort,
		Health:    httpHealth("/", 0),
		Notes: []string{
			"A static bundle: there is no application server, so nginx serves the build output.",
			"nginx has no environment interpolation, so the listen directive is templated with envsubst at start.",
		},
		Dockerfile: `FROM node:24-alpine AS build
WORKDIR /app
COPY package.json package-lock.json* ./
RUN if [ -f package-lock.json ]; then npm ci; else npm install --no-audit --no-fund; fi
COPY . .
RUN npm run build

FROM nginx:alpine
RUN rm /etc/nginx/conf.d/default.conf
COPY --from=build /app/dist /usr/share/nginx/html
RUN mkdir -p /etc/nginx/templates && printf '%s\n' \
  'server {' \
  '  listen ${PORT};' \
  '  root /usr/share/nginx/html;' \
  '  location / { try_files $uri $uri/ /index.html; }' \
  '}' > /etc/nginx/templates/default.conf.template
ENV PORT=8080
EXPOSE 8080
CMD ["sh", "-c", "envsubst '$PORT' < /etc/nginx/templates/default.conf.template > /etc/nginx/conf.d/default.conf && nginx -g 'daemon off;'"]
`,
	}
}

func django(dir string) Recipe {
	project := findWSGIProject(dir)
	if project == "" {
		project = "app"
	}
	collectstatic := hasStaticRoot(dir, project)
	start := "python manage.py migrate --noinput"
	if collectstatic {
		start += " && python manage.py collectstatic --noinput"
	}
	start += fmt.Sprintf(" && gunicorn %s.wsgi:application --bind 0.0.0.0:${PORT:-8080}", project)

	staticNote := "STATIC_ROOT is not set, so collectstatic is omitted: on a bare project it would fail the start."
	if collectstatic {
		staticNote = "STATIC_ROOT is set, so collectstatic runs at start."
	}
	return Recipe{
		Framework: FrameworkDjango,
		Port:      AppPort,
		Health:    httpHealth("/", 30),
		Notes: []string{
			"ALLOWED_HOSTS must include the machine's hostname (['*'] or os.environ['DJANGO_ALLOWED_HOSTS'].split(',')), or every request is a 400 DisallowedHost.",
			staticNote,
			"The migration runs at start here. For a service with replicas, move it to x-pilots.pre_deploy so it runs once rather than once per replica.",
		},
		Dockerfile: `FROM python:3.12-slim
ENV PYTHONDONTWRITEBYTECODE=1 PYTHONUNBUFFERED=1 PORT=8080
WORKDIR /app
# Detection accepts either file, so the install has to as well: a COPY of a
# requirements.txt a Poetry project does not have fails the build before pip
# has said anything about dependencies.
COPY requirements.txt* pyproject.toml* ./
RUN pip install --no-cache-dir gunicorn && \
  if [ -f requirements.txt ]; then pip install --no-cache-dir -r requirements.txt; else pip install --no-cache-dir .; fi
COPY . .
EXPOSE 8080
CMD ["sh", "-c", "` + start + `"]
`,
	}
}

func fastapi(dir string) Recipe {
	module := "app"
	if _, err := os.Stat(filepath.Join(dir, "main.py")); err == nil {
		module = "main"
	}
	return Recipe{
		Framework: FrameworkFastAPI,
		Port:      AppPort,
		Health:    httpHealth("/", 0),
		Notes: []string{
			"The ASGI app is taken as " + module + ":app; rename the target if the callable is not called `app`.",
			"Detection is the fastapi import alone, so nothing promises a requirements.txt: the install takes requirements.txt, then pyproject.toml, and installs only uvicorn if the repo declares its dependencies somewhere else.",
		},
		Dockerfile: `FROM python:3.12-slim
ENV PYTHONDONTWRITEBYTECODE=1 PYTHONUNBUFFERED=1 PORT=8080
WORKDIR /app
COPY . .
RUN pip install --no-cache-dir uvicorn && \
  if [ -f requirements.txt ]; then pip install --no-cache-dir -r requirements.txt; \
  elif [ -f pyproject.toml ]; then pip install --no-cache-dir .; fi
EXPOSE 8080
CMD ["sh", "-c", "uvicorn ` + module + `:app --host 0.0.0.0 --port ${PORT:-8080}"]
`,
	}
}

func rails() Recipe {
	return Recipe{
		Framework: FrameworkRails,
		Port:      AppPort,
		Health:    httpHealth("/up", 0),
		Notes: []string{
			"Rails 7.1 and newer serve /up as a health endpoint, which is what the check polls.",
			"SECRET_KEY_BASE has to be set for a production boot; pass it as a sealed environment variable.",
			// Turbo is not a separate framework and needs no recipe of its own:
			// it is a gem inside the Rails app. But its broadcasts ride Action
			// Cable, and Rails' generated config/cable.yml uses the redis
			// adapter in production, defaulted to redis://localhost:6379/1 --
			// an address that answers nothing inside a machine. The failure is
			// silent in the worst way: the page renders, the deploy is green,
			// and no Turbo Stream ever arrives.
			"Turbo Streams broadcast over Action Cable, and the generated config/cable.yml uses the redis adapter in production at redis://localhost:6379/1. Nothing listens there inside a machine, so broadcasts are dropped with the page still rendering fine. Point REDIS_URL at a redis service, or switch the production adapter to solid_cable (database-backed) or async (single process only).",
			"WebSocket upgrades and SSE both reach the app unbuffered, so Action Cable at /cable and a Turbo Stream over SSE work once the adapter above is right.",
			// The other classic way a Rails app fails on a platform: force_ssl
			// with no proxy header is an infinite redirect, and the health
			// check polls /up over http, so the gate never opens.
			"config.force_ssl = true is safe here: the router sets X-Forwarded-Proto, so Rails sees the request as SSL instead of redirecting to itself forever.",
		},
		Dockerfile: `FROM ruby:3.3-slim
RUN apt-get update && apt-get install -y --no-install-recommends build-essential libpq-dev ca-certificates && rm -rf /var/lib/apt/lists/*
ENV RAILS_ENV=production RAILS_LOG_TO_STDOUT=1 PORT=8080
WORKDIR /app
COPY Gemfile Gemfile.lock ./
RUN bundle install
COPY . .
RUN SECRET_KEY_BASE=build-only bundle exec rake assets:precompile
EXPOSE 8080
CMD ["sh", "-c", "bundle exec puma -b tcp://0.0.0.0:${PORT:-8080}"]
`,
	}
}

func golang() Recipe {
	return Recipe{
		Framework: FrameworkGo,
		Port:      AppPort,
		Health:    httpHealth("/", 0),
		Notes: []string{
			"The binary must read PORT from the environment and listen on 0.0.0.0 (or \":\"+port, which binds every interface). A hard-coded 127.0.0.1 builds cleanly and answers 502.",
			"The runtime stage is distroless, so there is no shell in the image: `exec` into this machine will not find /bin/sh.",
			"The binary is the FIRST main package `go list ./...` reports, so a module whose entry point is under ./cmd/<name> builds without editing this file. `go build -o` cannot take `./...` directly: it refuses to write more than one package to one path.",
		},
		Dockerfile: `FROM golang:1.23 AS build
WORKDIR /src
COPY go.mod go.sum* ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /app $(go list -f '{{if eq .Name "main"}}{{.ImportPath}}{{end}}' ./... | grep -v '^$' | head -n 1)

FROM gcr.io/distroless/static
COPY --from=build /app /app
ENV PORT=8080
EXPOSE 8080
CMD ["/app"]
`,
	}
}

func rust(dir string) Recipe {
	name := cargoPackageName(dir)
	if name == "" {
		name = "app"
	}
	return Recipe{
		Framework: FrameworkRust,
		Port:      AppPort,
		Health:    httpHealth("/", 0),
		Notes: []string{
			"The binary must read PORT from the environment and bind 0.0.0.0. A hard-coded 127.0.0.1 builds cleanly and answers 502.",
			"The release binary is taken as target/release/" + name + ", from the package name in Cargo.toml.",
		},
		Dockerfile: `FROM rust:1-slim AS build
WORKDIR /src
COPY . .
RUN cargo build --release

FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates && rm -rf /var/lib/apt/lists/*
COPY --from=build /src/target/release/` + name + ` /app
ENV PORT=8080
EXPOSE 8080
CMD ["/app"]
`,
	}
}

func laravel() Recipe {
	return Recipe{
		Framework: FrameworkLaravel,
		Port:      AppPort,
		Health:    httpHealth("/", 0),
		Notes: []string{
			"APP_KEY has to be set, or every boot fails; pass it as a sealed environment variable.",
			"`artisan serve` is PHP's built-in server and is single-threaded. For real traffic, swap it for php-fpm behind nginx.",
		},
		Dockerfile: `FROM php:8.3-cli
RUN apt-get update && apt-get install -y --no-install-recommends git unzip libpq-dev && docker-php-ext-install pdo_pgsql && rm -rf /var/lib/apt/lists/*
COPY --from=composer:2 /usr/bin/composer /usr/bin/composer
ENV PORT=8080
WORKDIR /app
COPY composer.json composer.lock* ./
RUN composer install --no-dev --no-scripts --no-autoloader
COPY . .
RUN composer dump-autoload --optimize
EXPOSE 8080
CMD ["sh", "-c", "php artisan serve --host=0.0.0.0 --port=${PORT:-8080}"]
`,
	}
}
