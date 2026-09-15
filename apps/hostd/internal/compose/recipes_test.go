package compose

import (
	"sort"
	"strings"
	"testing"

	yaml "go.yaml.in/yaml/v3"
)

// Every recipe is PRIVATE and carries a snapshot policy.
//
// Both are decisions the operator would otherwise have to make, and both have
// an expensive wrong answer: a public database is a port exposed to the
// internet, and a database with no backup policy is the single most common way
// a self-hosted platform loses somebody's data.
func TestEveryRecipeIsPrivateAndBackedUp(t *testing.T) {
	for _, engine := range Engines {
		t.Run(engine, func(t *testing.T) {
			r, err := Generate(engine, "", "", true)
			if err != nil {
				t.Fatalf("Generate(%s): %v", engine, err)
			}
			x, ok := r.Service["x-pilots"].(map[string]any)
			if !ok {
				t.Fatal("the block carries no x-pilots")
			}
			if x["private"] != true {
				t.Error("the database is not private; it would be given a public URL that times out")
			}
			if x["engine"] != engine {
				t.Errorf("engine label = %v, want %s", x["engine"], engine)
			}
			snaps, ok := x["snapshots"].(map[string]any)
			if !ok || snaps["cron"] == "" {
				t.Error("the recipe schedules no snapshots")
			}
			if snaps["keep_daily"] == 0 && snaps["keep_weekly"] == 0 {
				t.Error("the recipe keeps nothing; a schedule with no retention grows without bound")
			}
		})
	}
}

// Every recipe names its secret rather than carrying a value. The password is
// generated on the caller's machine and never crosses the wire, so the
// generator cannot be the thing that knows it.
func TestARecipeCarriesNoPassword(t *testing.T) {
	for _, engine := range Engines {
		r, err := Generate(engine, "", "", true)
		if err != nil {
			t.Fatal(err)
		}
		if len(r.SecretNames) == 0 {
			t.Errorf("%s names no secret", engine)
		}
		// The environment references the secret by name, through the
		// `secret://` scheme the planner resolves.
		env, _ := r.Service["environment"].(map[string]any)
		refs := 0
		for _, v := range env {
			if s, ok := v.(string); ok && strings.HasPrefix(s, "secret://") {
				refs++
			}
		}
		if refs == 0 {
			t.Errorf("%s's environment carries no secret:// reference: %v", engine, env)
		}
		// And the template is a template, not a filled-in URL.
		if !strings.Contains(r.URLTemplate, "PASSWORD") {
			t.Errorf("%s's URL template has no placeholder: %s", engine, r.URLTemplate)
		}
	}
}

// The two Postgres modes make different durability trades, and each says which
// out loud. A durability decision the operator did not read is a decision they
// did not make.
func TestBothPostgresModesStateTheirTrade(t *testing.T) {
	wal, err := Generate("postgres", "", ModeWALArchive, false)
	if err != nil {
		t.Fatal(err)
	}
	durable, err := Generate("postgres", "", ModeDurableVolume, false)
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(wal.Statement, "60 seconds") {
		t.Errorf("the wal-archive statement does not say what can be lost: %q", wal.Statement)
	}
	if !strings.Contains(durable.Statement, "nothing is lost") {
		t.Errorf("the durable-volume statement does not say what it guarantees: %q", durable.Statement)
	}
	// And they are genuinely different configurations, not one with a
	// different sentence attached.
	if wal.Service["image"] != nil {
		t.Error("the wal-archive mode uses a stock image; it needs a build for the base backup")
	}
	if durable.Service["build"] != nil {
		t.Error("the durable-volume mode builds an image it does not need")
	}
	// The default is wal-archive: the expensive choice should be chosen rather
	// than inherited.
	def, err := Generate("postgres", "", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if def.Mode != ModeWALArchive {
		t.Errorf("the default mode is %s, want %s", def.Mode, ModeWALArchive)
	}
}

// The archive command is a LIST. It contains spaces and a `&&`, and a string
// would be re-split by whatever ran it -- which fails as postgres refusing to
// start, with nothing naming the quoting.
func TestThePostgresCommandIsAList(t *testing.T) {
	r, err := Generate("postgres", "", ModeWALArchive, false)
	if err != nil {
		t.Fatal(err)
	}
	cmd, ok := r.Service["command"].([]string)
	if !ok {
		t.Fatalf("command is %T, want a list", r.Service["command"])
	}
	joined := strings.Join(cmd, " ")
	if !strings.Contains(joined, "archive_mode=on") {
		t.Errorf("archiving is not on: %v", cmd)
	}
	if !strings.Contains(joined, "archive_timeout=60") {
		t.Errorf("no archive timeout, so a quiet database ships nothing: %v", cmd)
	}
}

// Naming the service changes the address the connection string points at, and
// the build context with it. A recipe whose URL named the engine rather than
// the service would point at the wrong machine the moment somebody used --name.
func TestANamedDatabaseIsAddressedByItsName(t *testing.T) {
	r, err := Generate("postgres", "orders", ModeWALArchive, false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(r.URLTemplate, "orders.internal") {
		t.Errorf("the URL points at %q, not at orders.internal", r.URLTemplate)
	}
	if r.Service["build"] != "./.pilots/orders" {
		t.Errorf("the build context is %v, want ./.pilots/orders", r.Service["build"])
	}
	if _, ok := r.Files[".pilots/orders/Dockerfile"]; !ok {
		t.Errorf("no Dockerfile at the named context: %v", r.Files)
	}
}

// Redis is appendonly, because the default is a periodic dump and a Redis
// restarted between dumps comes back having lost whatever happened since. A
// cache can afford that; a queue or a lock cannot, and the recipe does not get
// to decide which this is.
func TestRedisPersistsEveryWrite(t *testing.T) {
	r, err := Generate("redis", "", "", false)
	if err != nil {
		t.Fatal(err)
	}
	cmd, _ := r.Service["command"].([]string)
	if !strings.Contains(strings.Join(cmd, " "), "--appendonly yes") {
		t.Errorf("redis is not appendonly: %v", cmd)
	}
	if !strings.Contains(strings.Join(cmd, " "), "requirepass") {
		t.Errorf("redis takes no password: %v", cmd)
	}
}

// An engine with no recipe is refused by name, and the refusal says what there
// IS. A guessed recipe is a guess about somebody's production database.
func TestAnUnknownEngineIsRefusedWithTheList(t *testing.T) {
	_, err := Generate("cassandra", "", "", false)
	if err == nil {
		t.Fatal("an engine with no recipe was generated anyway")
	}
	for _, engine := range Engines {
		if !strings.Contains(err.Error(), engine) {
			t.Errorf("the refusal does not mention %s: %v", engine, err)
		}
	}
}

// Only Postgres has a WAL archive worth configuring. Offering the mode for the
// others would be offering a durability story we have not built.
func TestOnlyPostgresHasTwoModes(t *testing.T) {
	for _, engine := range []string{"mysql", "redis", "mongo"} {
		if _, err := Generate(engine, "", ModeWALArchive, false); err == nil {
			t.Errorf("%s accepted a wal-archive mode it has no archive for", engine)
		}
		r, err := Generate(engine, "", ModeDurableVolume, false)
		if err != nil {
			t.Fatalf("%s: %v", engine, err)
		}
		if r.Mode != ModeDurableVolume {
			t.Errorf("%s came back as %s", engine, r.Mode)
		}
	}
}

// The password goes into the URL where the placeholder was, once.
func TestTheURLIsFilledInWithTheGeneratedPassword(t *testing.T) {
	r, err := Generate("postgres", "", ModeDurableVolume, false)
	if err != nil {
		t.Fatal(err)
	}
	got := r.URLFor("s3cr3t")
	if strings.Contains(got, "PASSWORD") {
		t.Errorf("the placeholder survived: %s", got)
	}
	if !strings.Contains(got, "s3cr3t") {
		t.Errorf("the password is not in the URL: %s", got)
	}
}

// The pooler must cost no second machine.
//
// This is the whole design: a pooler on its own machine adds a network hop to
// every query and a thing that can be down while the database is up. Written as
// a compose service on the SAME build context, the planner folds it into the
// database's own machine as a second process. If that ever stops being true the
// pooler silently becomes a second machine, which still works and is still
// wrong, so it is asserted rather than assumed.
func TestThePoolerRunsInsideTheDatabasesOwnMachine(t *testing.T) {
	r, err := Generate("postgres", "pg", ModeWALArchive, true)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if len(r.Companions) != 1 {
		t.Fatalf("companions = %v, want exactly the pooler", r.Companions)
	}
	pool, ok := r.Companions["pg-pool"]
	if !ok {
		t.Fatalf("companions = %v, want one called pg-pool", r.Companions)
	}
	// Same build context and same (absent) Dockerfile is exactly the grouping
	// rule in group.go. Different values here mean two machines.
	if pool["build"] != r.Service["build"] {
		t.Errorf("pooler builds %v, database builds %v: they must share a context "+
			"or the planner gives them a machine each", pool["build"], r.Service["build"])
	}
	// The pooler must NOT publish. Only one process owns a machine's port, and
	// it has to be the database, whose pg_isready is the honest health gate.
	if _, published := pool["ports"]; published {
		t.Error("the pooler publishes a port; the database owns the machine's port")
	}

	text := "name: demo\nservices:\n" +
		"  pg:\n" + indentYAML(t, r.Service) +
		"  pg-pool:\n" + indentYAML(t, pool) +
		"volumes:\n  pgarchive: {}\n"
	plan, planErr := planText(t, text)
	if planErr != nil {
		t.Fatalf("the recipe's own output does not plan: %+v", planErr)
	}
	if len(plan.Steps) != 1 {
		t.Fatalf("got %d steps, want one machine holding both processes", len(plan.Steps))
	}
	step := plan.Steps[0]
	if step.Name != "pg" {
		t.Errorf("machine name = %q, want pg: the machine's name is its address, "+
			"so the database must be the surviving member", step.Name)
	}
	if len(step.Processes) != 2 {
		t.Fatalf("processes = %+v, want the database and the pooler", step.Processes)
	}
	for _, proc := range step.Processes {
		if proc.Name != "pg-pool" {
			continue
		}
		if proc.Port {
			t.Error("the pooler claims the machine's port")
		}
		if len(proc.Needs) != 1 || proc.Needs[0] != "pg" {
			t.Errorf("pooler needs = %v, want [pg]: a pooler with no database "+
				"behind it accepts connections and fails them", proc.Needs)
		}
	}
}

// The pooler's configuration must not be baked into the image, because it holds
// the database password and an image is a thing that gets pushed and cached.
func TestThePoolersPasswordIsWrittenAtStartAndNotBuiltIn(t *testing.T) {
	r, err := Generate("postgres", "pg", ModeWALArchive, true)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	for path, body := range r.Files {
		if strings.HasSuffix(path, "Dockerfile") && strings.Contains(body, "userlist") {
			t.Errorf("%s writes the pgbouncer userlist at build time; the password "+
				"would travel with the image", path)
		}
	}
	start, ok := r.Files[".pilots/pg/pilot-pgbouncer.sh"]
	if !ok {
		t.Fatal("no start script; the pooler has nothing to run")
	}
	if !strings.Contains(start, "POSTGRES_PASSWORD") {
		t.Error("the start script never reads the password, so the userlist it writes is empty")
	}
	if !strings.Contains(start, "umask 077") {
		t.Error("the userlist is written before the umask narrows; it would be " +
			"world-readable for however long the chmod takes")
	}
	if !strings.Contains(start, "pool_mode = transaction") {
		t.Error("session mode pools almost nothing: a client holding its connection " +
			"holds a server connection with it, which is the problem")
	}
}

// Durable-volume mode otherwise runs the stock image. Asking for a pooler has
// to give it a build context, or the two services never group.
func TestPoolingADurableVolumeDatabaseGivesItABuildContext(t *testing.T) {
	r, err := Generate("postgres", "pg", ModeDurableVolume, true)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if _, stock := r.Service["image"]; stock {
		t.Error("the database still runs the stock image, so it shares no build " +
			"context with the pooler and they become two machines")
	}
	if r.Service["build"] != "./.pilots/pg" {
		t.Errorf("build = %v, want ./.pilots/pg", r.Service["build"])
	}
	if _, ok := r.Files[".pilots/pg/Dockerfile"]; !ok {
		t.Error("no Dockerfile, so there is nothing to build")
	}
}

// indentYAML renders a recipe block as the body of a compose service, indented
// to sit under `services:`. Used to plan a recipe's OWN output rather than a
// hand-written approximation of it, which is the only version worth asserting.
func indentYAML(t *testing.T, block map[string]any) string {
	t.Helper()
	raw, err := yaml.Marshal(block)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var b strings.Builder
	for _, line := range strings.Split(strings.TrimRight(string(raw), "\n"), "\n") {
		b.WriteString("    " + line + "\n")
	}
	return b.String()
}

// Every recipe's own output must survive the planner.
//
// This is the test the recipes needed from the start and did not have. Each one
// was asserted field by field -- the right image, the right volume, the right
// statement -- and every one of those assertions passed on a file that
// `pilot deploy` refused to plan. Two separate reasons, both invisible to a
// field-by-field check:
//
//   - every recipe published its engine's port, and a file that publishes ports
//     and not 8080 describes a service the router can never reach;
//   - Redis's healthcheck and command spelled the password `$REDIS_PASSWORD`,
//     which compose substitutes when it PARSES the file, from an environment
//     that does not have it. The file failed to load at all.
//
// So the assertion is the whole round trip: generate the fragment, write it as
// the compose file it is meant to become, and plan it. Nothing a recipe can
// emit is exempt, which is why it loops over Engines rather than naming them.
func TestEveryRecipePlans(t *testing.T) {
	for _, engine := range Engines {
		for _, pool := range []bool{false, true} {
			if pool && engine != "postgres" {
				continue
			}
			name := engine
			if pool {
				name += " with a pooler"
			}
			t.Run(name, func(t *testing.T) {
				r, err := Generate(engine, "db", "", pool)
				if err != nil {
					t.Fatalf("Generate: %v", err)
				}
				text := "name: demo\nservices:\n  db:\n" + indentYAML(t, r.Service)
				for _, companion := range sortedCompanions(r) {
					text += "  " + companion + ":\n" + indentYAML(t, r.Companions[companion])
				}
				if len(r.Volumes) > 0 {
					text += "volumes:\n"
					for volume := range r.Volumes {
						text += "  " + volume + ": {}\n"
					}
				}
				plan, planErr := planText(t, text)
				if planErr != nil {
					t.Fatalf("the recipe does not plan, so `pilot add %s` writes a "+
						"file `pilot deploy` refuses: %+v", engine, planErr)
				}
				if len(plan.Steps) != 1 {
					t.Fatalf("got %d steps, want one machine", len(plan.Steps))
				}
				// The health gate must survive too. A database that plans but
				// carries no gate is a database a broken release rolls past.
				if h := plan.Steps[0].Health; h == nil || h.Type != "cmd" {
					t.Errorf("health = %+v, want a cmd gate: a database has nothing "+
						"to answer on the router's port, so the command is the gate", h)
				}
			})
		}
	}
}

// A database publishes nothing, and that is deliberate rather than an omission.
func TestNoRecipePublishesAPort(t *testing.T) {
	for _, engine := range Engines {
		r, err := Generate(engine, "db", "", false)
		if err != nil {
			t.Fatalf("Generate: %v", err)
		}
		if _, published := r.Service["ports"]; published {
			t.Errorf("%s publishes a port. The router dials container port 8080 and "+
				"a database has nothing to answer there, so publishing its engine "+
				"port describes a service the router can never reach. Peers already "+
				"dial db.internal:<port> over the mesh without it", engine)
		}
	}
}

// sortedCompanions keeps the generated file stable between runs.
func sortedCompanions(r *Recipe) []string {
	out := make([]string, 0, len(r.Companions))
	for name := range r.Companions {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}
