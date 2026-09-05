package compose

import (
	"context"
	"strings"
	"testing"
)

// fixture is packages/cli/test/fixtures/compose-app/compose.yaml, inlined. The
// CLI's own deploy test asserts what it does with the plan for this file; this
// asserts that the plan it gets is that one.
const fixture = `# The shop app: a database, a web service and a worker.
services:
  postgres:
    image: postgres:17
  web:
    build: ./web
    environment:
      DATABASE_URL: secret://database_url
    x-pilots:
      pre_deploy: python manage.py migrate --noinput
  worker:
    build: ./worker
    depends_on:
      - postgres
`

// shop plans the fixture as the CLI does, naming the app through the
// environment.
func shop(t *testing.T) *Plan {
	t.Helper()
	plan, planErr, err := Compile(context.Background(), Request{
		Compose: fixture,
		Env:     map[string]string{"COMPOSE_PROJECT_NAME": "shop"},
	})
	if err != nil || planErr != nil {
		t.Fatalf("planning the fixture: err=%v planErr=%+v", err, planErr)
	}
	return plan
}

func names(p *Plan) []string {
	out := make([]string, 0, len(p.Steps))
	for _, s := range p.Steps {
		out = append(out, s.Name)
	}
	return out
}

func stepNamed(t *testing.T, p *Plan, name string) Step {
	t.Helper()
	for _, s := range p.Steps {
		if s.Name == name {
			return s
		}
	}
	t.Fatalf("no step %s in %v", name, names(p))
	return Step{}
}

// The order is a function of the file and of nothing else. compose-go's own
// graph walk is concurrent, so an order taken from it would depend on
// goroutine scheduling -- two hosts would build the same app differently, and
// the difference would show up as a flake nobody could reproduce.
func TestTheFixturePlansInTheSameOrderEveryTime(t *testing.T) {
	want := []string{"postgres", "web", "worker"}
	for i := range 20 {
		got := names(shop(t))
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("run %d planned %v, want %v", i, got, want)
		}
	}
}

func TestTheFixtureMapsEachServiceOntoItsPrimitives(t *testing.T) {
	plan := shop(t)
	if plan.App != "shop" {
		t.Errorf("app = %q, want shop", plan.App)
	}

	// A stock image is a build too: hostd turns any Dockerfile into a rootfs,
	// so the plan hands the CLI a one-line one rather than a second code path.
	pg := stepNamed(t, plan, "postgres")
	if pg.Dockerfile != "FROM postgres:17\n" {
		t.Errorf("postgres dockerfile = %q", pg.Dockerfile)
	}
	if pg.Build != nil {
		t.Errorf("postgres carries a build context: %+v", pg.Build)
	}

	web := stepNamed(t, plan, "web")
	if web.Build == nil || web.Build.Context != "./web" {
		t.Errorf("web build = %+v, want the path relative to the compose file", web.Build)
	}
	// The secret is NAMED, never carried: the CLI resolves it from the
	// operator's own store and hostd seals what comes back.
	if web.SecretRefs["DATABASE_URL"] != "database_url" {
		t.Errorf("web secret_refs = %v", web.SecretRefs)
	}
	if len(web.Env) != 0 {
		t.Errorf("a secret:// value reached env: %v", web.Env)
	}
	if web.PreDeploy != "python manage.py migrate --noinput" {
		t.Errorf("web pre_deploy = %q", web.PreDeploy)
	}

	worker := stepNamed(t, plan, "worker")
	if len(worker.DependsOn) != 1 || worker.DependsOn[0] != "postgres" {
		t.Errorf("worker depends_on = %v", worker.DependsOn)
	}
	// The machine defaults, since the file names none.
	if worker.Replicas != 1 || worker.VCPUs != 1 || worker.MemMiB != 512 {
		t.Errorf("worker defaults = %d replicas, %d vcpus, %d MiB",
			worker.Replicas, worker.VCPUs, worker.MemMiB)
	}
}

// compose-go's own behaviour for an unset variable is a warning and a blank. A
// blank DATABASE_URL deployed is worse than a refusal: the app comes up,
// connects to nothing, and the first anyone hears of it is a 500.
func TestAnUnsetVariableIsRefusedByName(t *testing.T) {
	const file = `
name: shop
services:
  web:
    image: nginx
    environment:
      A: ${MISSING}
      B: ${ALSO_MISSING}
`
	_, _, err := Compile(context.Background(), Request{Compose: file})
	if err == nil {
		t.Fatal("an unset variable planned successfully")
	}
	for _, want := range []string{"MISSING", "ALSO_MISSING"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %s", err, want)
		}
	}
}

func TestAVariableWithADefaultOrAValueIsNotRefused(t *testing.T) {
	for _, tc := range []struct {
		name, file string
		env        map[string]string
	}{
		{"a default", "name: shop\nservices:\n  web:\n    image: ${TAG:-nginx}\n", nil},
		{"a presence value", "name: shop\nservices:\n  web:\n    image: nginx${X:+-alpine}\n", nil},
		{"a value in the request env", "name: shop\nservices:\n  web:\n    image: ${IMG}\n",
			map[string]string{"IMG": "nginx"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := Compile(context.Background(), Request{Compose: tc.file, Env: tc.env}); err != nil {
				t.Errorf("planned with an error: %v", err)
			}
		})
	}
}

// COMPOSE_PROJECT_NAME beats x-pilots.app beats name:, which is compose's own
// order: the environment overrides the file.
func TestTheAppNamePrecedence(t *testing.T) {
	const withBoth = `
name: from-name
x-pilots:
  app: from-x-pilots
services:
  web:
    image: nginx
`
	for _, tc := range []struct {
		name, file string
		env        map[string]string
		want       string
	}{
		{"the environment wins", withBoth, map[string]string{"COMPOSE_PROJECT_NAME": "from-env"}, "from-env"},
		{"then x-pilots.app", withBoth, nil, "from-x-pilots"},
		{"then name:", "name: from-name\nservices:\n  web:\n    image: nginx\n", nil, "from-name"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plan, planErr, err := Compile(context.Background(), Request{Compose: tc.file, Env: tc.env})
			if err != nil || planErr != nil {
				t.Fatalf("err=%v planErr=%+v", err, planErr)
			}
			if plan.App != tc.want {
				t.Errorf("app = %q, want %q", plan.App, tc.want)
			}
		})
	}
}

// A default app name would merge two nameless apps into one .internal
// namespace, where each one's services would start resolving the other's.
func TestANamelessFileIsRefused(t *testing.T) {
	_, _, err := Compile(context.Background(), Request{
		Compose: "services:\n  web:\n    image: nginx\n",
	})
	if err == nil {
		t.Fatal("a nameless file planned successfully")
	}
	if !strings.Contains(err.Error(), "COMPOSE_PROJECT_NAME") {
		t.Errorf("error %q does not say how to fix it", err)
	}
}

// env_file is caught in the pre-scan, BEFORE loading. compose-go reads every
// env_file from disk while it loads and a missing one is a load error, so a
// check on the loaded model would never run on a server that has no files.
func TestEnvFileIsRefusedBeforeLoading(t *testing.T) {
	const file = `
name: shop
services:
  web:
    image: nginx
    env_file: .env
`
	plan, planErr, err := Compile(context.Background(), Request{Compose: file})
	if plan != nil || err != nil {
		t.Fatalf("plan=%v err=%v, want a PlanError", plan, err)
	}
	if planErr.Error != unsupportedError {
		t.Errorf("error = %q, want %q", planErr.Error, unsupportedError)
	}
	if len(planErr.Unsupported) != 1 || planErr.Unsupported[0].Key != "env_file" {
		t.Fatalf("unsupported = %+v", planErr.Unsupported)
	}
	if !strings.Contains(planErr.Unsupported[0].Message, "secret://") {
		t.Errorf("the message does not say what to do instead: %q",
			planErr.Unsupported[0].Message)
	}
}

// One 400 carrying every offending key, so a file is fixed in one pass rather
// than one key per failed deploy.
func TestEveryUnsupportedKeyArrivesInOneError(t *testing.T) {
	const file = `
name: shop
services:
  db:
    image: postgres:17
  web:
    image: nginx
    privileged: true
    security_opt:
      - seccomp:unconfined
    depends_on:
      db:
        condition: service_completed_successfully
    deploy:
      placement:
        constraints:
          - node.role == manager
`
	plan, planErr, err := Compile(context.Background(), Request{Compose: file})
	if plan != nil || err != nil {
		t.Fatalf("plan=%v err=%v, want a PlanError", plan, err)
	}
	if planErr.Error != unsupportedError {
		t.Errorf("error = %q, want %q", planErr.Error, unsupportedError)
	}

	want := map[string]string{
		"privileged":       msgMoot,
		"security_opt":     msgUnsupported,
		"deploy.placement": "placement is decided by the fleet",
		"depends_on.condition": "service_completed_successfully is not supported; " +
			"use x-pilots.pre_deploy",
	}
	got := map[string]string{}
	for _, u := range planErr.Unsupported {
		if u.Service != "web" {
			t.Errorf("unexpected service in %+v", u)
		}
		got[u.Key] = u.Message
	}
	for key, message := range want {
		if got[key] != message {
			t.Errorf("%s: message = %q, want %q", key, got[key], message)
		}
	}
	if len(got) != len(want) {
		t.Errorf("collected %v, want exactly %v", got, want)
	}
	// Sorted, so two runs answer identically.
	for i := 1; i < len(planErr.Unsupported); i++ {
		if planErr.Unsupported[i-1].Key > planErr.Unsupported[i].Key {
			t.Errorf("the list is not sorted: %+v", planErr.Unsupported)
			break
		}
	}
}

// A bind mount has no host to bind to. A named volume is a real disk in object
// storage, and it is what the file should have said.
func TestABindMountIsRefusedAndANamedVolumeIsNot(t *testing.T) {
	const bind = `
name: shop
services:
  db:
    image: postgres:17
    volumes:
      - ./data:/data
`
	_, planErr, err := Compile(context.Background(), Request{Compose: bind})
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if planErr == nil || planErr.Unsupported[0].Key != "volumes" {
		t.Fatalf("a bind mount planned: %+v", planErr)
	}
	if !strings.Contains(planErr.Unsupported[0].Message, "named volume") {
		t.Errorf("the message does not say what to use: %q", planErr.Unsupported[0].Message)
	}

	const named = `
name: shop
volumes:
  pgdata: {}
services:
  db:
    image: postgres:17
    volumes:
      - pgdata:/var/lib/postgresql/data
`
	plan, planErr, err := Compile(context.Background(), Request{Compose: named})
	if err != nil || planErr != nil {
		t.Fatalf("a named volume was refused: err=%v planErr=%+v", err, planErr)
	}
	vols := stepNamed(t, plan, "db").Volumes
	if len(vols) != 1 || vols[0].Name != "pgdata" || vols[0].MountPath != "/var/lib/postgresql/data" {
		t.Fatalf("volumes = %+v", vols)
	}
	if vols[0].SizeGiB != defaultVolumeGiB {
		t.Errorf("size_gib = %d, want the default %d", vols[0].SizeGiB, defaultVolumeGiB)
	}
}

func TestXPilotsSizeGiBReachesEveryVolumeOfThatService(t *testing.T) {
	const file = `
name: shop
volumes:
  pgdata: {}
  pgarchive: {}
services:
  db:
    image: postgres:17
    volumes:
      - pgdata:/var/lib/postgresql/data
      - pgarchive:/archive
    x-pilots:
      size_gib: 4
`
	plan, planErr, err := Compile(context.Background(), Request{Compose: file})
	if err != nil || planErr != nil {
		t.Fatalf("err=%v planErr=%+v", err, planErr)
	}
	for _, v := range stepNamed(t, plan, "db").Volumes {
		if v.SizeGiB != 4 {
			t.Errorf("%s size_gib = %d, want 4", v.Name, v.SizeGiB)
		}
	}
}

// The knobs a step carries are the ones the deploy will apply, so they are
// built from the machine DEFAULTS rather than from zero: a struct assembled
// from zeros carries auto_start false, and a replica that suspends and then
// refuses to wake is a permanently dead URL earned by setting one unrelated
// field.
func TestXPilotsKnobsFillFromTheDefaults(t *testing.T) {
	const file = `
name: shop
services:
  db:
    image: postgres:17
    x-pilots:
      min_machines_running: 1
      auto_stop: "off"
      durable_volume: false
      whatever: 1
`
	plan, planErr, err := Compile(context.Background(), Request{Compose: file})
	if err != nil || planErr != nil {
		t.Fatalf("unknown x-pilots keys were not tolerated: err=%v planErr=%+v", err, planErr)
	}
	k := stepNamed(t, plan, "db").Knobs
	if k == nil {
		t.Fatal("knobs are nil for a file that set two of them")
	}
	if k.AutoStop != "off" || k.MinMachinesRunning != 1 {
		t.Errorf("knobs = %+v, want the file's values", *k)
	}
	// The two the file did not name come from DefaultKnobs, not from zero.
	if !k.AutoStart || k.SoftLimit != 20 {
		t.Errorf("knobs = %+v, want auto_start true and soft_limit 20", *k)
	}
}

// Nil, not a zero struct: the CLI spreads step.knobs onto a DeployRequest, and
// a service whose file says nothing about its lifecycle must keep whatever its
// previous release's replicas carried.
func TestAServiceWithNoKnobKeysCarriesNoKnobs(t *testing.T) {
	plan := shop(t)
	for _, s := range plan.Steps {
		if s.Knobs != nil {
			t.Errorf("%s carries knobs %+v for a file that names none", s.Name, *s.Knobs)
		}
	}
}

func TestAnUnknownAutoStopIsAnError(t *testing.T) {
	const file = `
name: shop
services:
  db:
    image: postgres:17
    x-pilots:
      auto_stop: sometimes
`
	_, _, err := Compile(context.Background(), Request{Compose: file})
	if err == nil || !strings.Contains(err.Error(), "auto_stop") {
		t.Fatalf("err = %v, want one naming auto_stop", err)
	}
}

// One element stays one argument. Splitting on spaces would turn the WAL
// archive command into five arguments and a shell operator the guest would
// then run.
func TestAListCommandIsQuotedPerElement(t *testing.T) {
	const file = `
name: shop
services:
  db:
    build: ./db
    command:
      - postgres
      - -c
      - archive_command=test ! -f /archive/wal/%f && cp %p /archive/wal/%f
`
	plan, planErr, err := Compile(context.Background(), Request{Compose: file})
	if err != nil || planErr != nil {
		t.Fatalf("err=%v planErr=%+v", err, planErr)
	}
	cmd := stepNamed(t, plan, "db").Cmd
	const want = `'postgres' '-c' 'archive_command=test ! -f /archive/wal/%f && cp %p /archive/wal/%f'`
	if cmd != want {
		t.Errorf("cmd = %s\nwant     %s", cmd, want)
	}
}

func TestASingleQuoteInAnArgumentSurvivesQuoting(t *testing.T) {
	if got := shellQuote(`it's`); got != `'it'\''s'` {
		t.Errorf("shellQuote = %s", got)
	}
}

// On an image: step the pair becomes Dockerfile lines instead, so a stock
// postgres that sets only command: keeps its OWN entrypoint -- and with it
// initdb, which is the only reason that image works without one.
func TestAnImageStepPutsCommandInTheDockerfileAndLeavesCmdEmpty(t *testing.T) {
	const file = `
name: shop
services:
  db:
    image: postgres:17
    command: [postgres, -c, x=1]
`
	plan, planErr, err := Compile(context.Background(), Request{Compose: file})
	if err != nil || planErr != nil {
		t.Fatalf("err=%v planErr=%+v", err, planErr)
	}
	db := stepNamed(t, plan, "db")
	const want = "FROM postgres:17\nCMD [\"postgres\",\"-c\",\"x=1\"]\n"
	if db.Dockerfile != want {
		t.Errorf("dockerfile = %q, want %q", db.Dockerfile, want)
	}
	if db.Cmd != "" {
		t.Errorf("cmd = %q, want it empty so the image keeps its entrypoint", db.Cmd)
	}
}

func TestAHealthcheckMapsToWholeSeconds(t *testing.T) {
	const file = `
name: shop
services:
  db:
    image: postgres:17
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -U postgres"]
      interval: 10s
      timeout: 3s
      start_period: 40s
      retries: 5
`
	plan, planErr, err := Compile(context.Background(), Request{Compose: file})
	if err != nil || planErr != nil {
		t.Fatalf("err=%v planErr=%+v", err, planErr)
	}
	h := stepNamed(t, plan, "db").Health
	if h == nil {
		t.Fatal("no health check")
	}
	if h.Type != "cmd" || h.IntervalSec != 10 || h.TimeoutSec != 3 ||
		h.GraceSec != 40 || h.HealthyThreshold != 5 {
		t.Errorf("health = %+v", *h)
	}
	if len(h.Test) != 2 || h.Test[0] != "CMD-SHELL" {
		t.Errorf("test = %v", h.Test)
	}
}

func TestADisabledHealthcheckIsTypeNone(t *testing.T) {
	const file = `
name: shop
services:
  db:
    image: postgres:17
    healthcheck:
      test: ["NONE"]
`
	plan, planErr, err := Compile(context.Background(), Request{Compose: file})
	if err != nil || planErr != nil {
		t.Fatalf("err=%v planErr=%+v", err, planErr)
	}
	if h := stepNamed(t, plan, "db").Health; h == nil || h.Type != "none" {
		t.Errorf("health = %+v, want type none", h)
	}
}

// A fractional cpu limit rounds UP: a microVM is given whole vCPUs, and
// rounding 1.5 down would quietly halve what the file asked for.
func TestResourceLimitsMapToWholeVCPUsAndMiB(t *testing.T) {
	const file = `
name: shop
services:
  web:
    image: nginx
    deploy:
      replicas: 3
      resources:
        limits:
          cpus: "1.5"
          memory: 1g
`
	plan, planErr, err := Compile(context.Background(), Request{Compose: file})
	if err != nil || planErr != nil {
		t.Fatalf("err=%v planErr=%+v", err, planErr)
	}
	web := stepNamed(t, plan, "web")
	if web.VCPUs != 2 {
		t.Errorf("vcpus = %d, want 1.5 rounded up", web.VCPUs)
	}
	if web.MemMiB != 1024 {
		t.Errorf("mem_mib = %d, want 1024", web.MemMiB)
	}
	if web.Replicas != 3 {
		t.Errorf("replicas = %d, want 3", web.Replicas)
	}
}

// A compose file carries these for local docker compose and they have a
// meaning-preserving reading here, so they are ignored rather than refused.
func TestKeysThatAreIgnoredRatherThanRefused(t *testing.T) {
	const file = `
name: shop
services:
  web:
    image: nginx
    restart: always
    container_name: web-1
    ports:
      - "8080:80"
    deploy:
      resources:
        reservations:
          cpus: "0.5"
`
	plan, planErr, err := Compile(context.Background(), Request{Compose: file})
	if err != nil || planErr != nil {
		t.Fatalf("err=%v planErr=%+v", err, planErr)
	}
	web := stepNamed(t, plan, "web")
	// Only the container port. The router owns 443 and there is no host to
	// publish on.
	if len(web.Ports) != 1 || web.Ports[0] != 80 {
		t.Errorf("ports = %v, want the container port alone", web.Ports)
	}
	// A reservation is a scheduling floor for a cluster with contention; a
	// microVM is given exactly what it is given.
	if web.VCPUs != 1 {
		t.Errorf("vcpus = %d, want the default -- a reservation is not a limit", web.VCPUs)
	}
}

func TestACycleIsRefusedNamingTheServicesOnIt(t *testing.T) {
	const file = `
name: shop
services:
  a:
    image: nginx
    depends_on: [b]
  b:
    image: nginx
    depends_on: [a]
`
	_, _, err := Compile(context.Background(), Request{Compose: file})
	if err == nil {
		t.Fatal("a cycle planned successfully")
	}
	for _, want := range []string{"cycle", "a", "b"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not carry %q", err, want)
		}
	}
}

func TestAMalformedFileIsAnError(t *testing.T) {
	for _, tc := range []struct{ name, file string }{
		{"not yaml", "services: [:::"},
		{"empty", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := Compile(context.Background(), Request{Compose: tc.file}); err == nil {
				t.Error("planned successfully")
			}
		})
	}
}

// The whole point of the ordering: a step never appears before something it
// depends on, whatever order the map happened to iterate in.
func TestADeepChainIsOrderedByDependency(t *testing.T) {
	const file = `
name: shop
services:
  z:
    image: nginx
    depends_on: [m]
  m:
    image: nginx
    depends_on: [a]
  a:
    image: nginx
`
	plan, planErr, err := Compile(context.Background(), Request{Compose: file})
	if err != nil || planErr != nil {
		t.Fatalf("err=%v planErr=%+v", err, planErr)
	}
	if got := strings.Join(names(plan), ","); got != "a,m,z" {
		t.Errorf("order = %s, want a,m,z", got)
	}
}
