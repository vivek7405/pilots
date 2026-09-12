package compose

import (
	"strings"
	"testing"
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
