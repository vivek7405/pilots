package cli

import (
	"strings"
	"testing"
)

// No engine's argv may carry the password.
//
// argv is world-readable in /proc on most systems, so a password on the command
// line is a password anybody with a shell on the machine can read, and one that
// lands in whatever ps output somebody pastes into a ticket. Every one of these
// four clients reads a password from the environment instead, which is the only
// reason this command can exist in this shape.
func TestNoClientTakesThePasswordOnItsCommandLine(t *testing.T) {
	const password = "hunter2-not-in-argv"
	for _, engine := range knownEngines() {
		eng := clientFor(engine)
		if eng == nil {
			t.Fatalf("%s has no client, but knownEngines lists it", engine)
		}
		if eng.PasswordEnv == "" {
			t.Errorf("%s names no password variable, so the password would have "+
				"nowhere to go but argv", engine)
		}
		argv := eng.Args("127.0.0.1", 15432, "postgres", "app")
		if strings.Contains(strings.Join(argv, " "), password) {
			t.Errorf("%s argv = %v, which carries the password", engine, argv)
		}
		if argv[0] != eng.Bin {
			t.Errorf("%s argv starts %q, want the binary %q", engine, argv[0], eng.Bin)
		}
	}
}

// The port the caller asks for is the port the client dials.
//
// This is the one thing a tunnel gets wrong invisibly: the client connects to
// the engine's default port on localhost, finds whatever is listening there --
// often a developer's own Postgres -- and the session succeeds against the
// wrong database.
func TestEveryClientDialsTheAddressItIsGiven(t *testing.T) {
	for _, engine := range knownEngines() {
		argv := strings.Join(clientFor(engine).Args("127.0.0.1", 45678, "root", "app"), " ")
		if !strings.Contains(argv, "45678") {
			t.Errorf("%s argv = %q, which never names the local port; it would "+
				"connect to whatever is on the default port instead", engine, argv)
		}
	}
}

// The direct URL wins over the pooled one.
//
// `pilot db connect` opens an interactive session, which is exactly the
// connection that should not go through a transaction pooler: a shell with no
// temporary tables, no session advisory locks and no LISTEN is a shell that
// behaves strangely for reasons nothing on screen explains.
func TestTheStoredDirectURLIsPreferredOverThePooledOne(t *testing.T) {
	store := secretStore{
		"demo": {
			"postgres_url":        "postgres://postgres:pw@db.internal:6432/postgres",
			"postgres_url_direct": "postgres://postgres:pw@db.internal:5432/postgres",
		},
	}
	if got := findDatabaseURL(store, "postgres_url"); !strings.Contains(got, "5432") {
		t.Errorf("findDatabaseURL = %q, want the direct address on 5432", got)
	}
	// With only the pooled one stored, that is the answer rather than nothing.
	delete(store["demo"], "postgres_url_direct")
	if got := findDatabaseURL(store, "postgres_url"); !strings.Contains(got, "6432") {
		t.Errorf("findDatabaseURL = %q, want the pooled address when it is all there is", got)
	}
}

// Two apps, one engine: the answer must not depend on map iteration order.
func TestTheURLLookupIsStableAcrossApps(t *testing.T) {
	store := secretStore{
		"alpha": {"postgres_url": "postgres://postgres:a@db.internal:5432/postgres"},
		"beta":  {"postgres_url": "postgres://postgres:b@db.internal:5432/postgres"},
	}
	first := findDatabaseURL(store, "postgres_url")
	for i := 0; i < 50; i++ {
		if got := findDatabaseURL(store, "postgres_url"); got != first {
			t.Fatalf("two runs disagree: %q then %q", first, got)
		}
	}
}

// A URL that cannot be read must send the caller down the remote path rather
// than stopping them. The remote path needs no password at all, so a parse
// failure is not a reason anybody should fail to connect.
func TestAnUnreadableURLYieldsNothingRatherThanAnError(t *testing.T) {
	for _, raw := range []string{"", "not a url at all", "postgres://db.internal:5432/postgres"} {
		user, database, password := splitDatabaseURL(raw)
		if user != "" || database != "" || password != "" {
			t.Errorf("splitDatabaseURL(%q) = %q/%q/%q, want empties", raw, user, database, password)
		}
	}
	user, database, password := splitDatabaseURL("postgres://postgres:pw@db.internal:5432/app")
	if user != "postgres" || database != "app" || password != "pw" {
		t.Errorf("got %q/%q/%q, want postgres/app/pw", user, database, password)
	}
}

// Only Postgres has a pooler, so only Postgres may name a pool port. A pool
// port on an engine with no pooler is a connection to a closed door.
func TestOnlyTheEngineWithAPoolerNamesAPoolPort(t *testing.T) {
	for _, engine := range knownEngines() {
		eng := clientFor(engine)
		if engine == "postgres" {
			if eng.PoolPort != 6432 {
				t.Errorf("postgres pool port = %d, want 6432", eng.PoolPort)
			}
			continue
		}
		if eng.PoolPort != 0 {
			t.Errorf("%s names pool port %d and has no pooler", engine, eng.PoolPort)
		}
	}
}
