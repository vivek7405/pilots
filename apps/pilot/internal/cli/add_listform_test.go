package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	yaml "go.yaml.in/yaml/v3"
)

// writeCompose puts a compose file on disk and hands back its path.
func composeFile(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "pilot-compose.yml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// parses reports whether the file is still a compose file at all.
func parsedCompose(t *testing.T, path string) map[string]any {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := yaml.Unmarshal(body, &out); err != nil {
		t.Fatalf("the file no longer parses as YAML: %v\n%s", err, body)
	}
	return out
}

// The LIST form of environment survives `pilot env set`.
//
// setInMap walked Content two at a time as key, value, key, value and appended
// a pair at the end, which is right for a mapping and corrupts anything else.
// Compose accepts environment in both forms and the list one is what most
// people write, so against that sequence the loop matched nothing and appended
// a bare scalar and an encoded value as two more LIST ITEMS -- turning a
// working compose file into one that no longer reads as compose, with no way
// back but git.
//
// Dropping the SequenceNode branch reds this: the file comes back with stray
// entries and the original variable unreadable.
func TestAListFormEnvironmentSurvivesAnEdit(t *testing.T) {
	path := composeFile(t, `name: shop
services:
  web:
    build: .
    environment:
      - DATABASE_URL=postgres://old
      - NODE_ENV=production
`)

	if err := addEnvTo(path, "web", "REDIS_URL", "redis://cache:6379"); err != nil {
		t.Fatalf("addEnvTo: %v", err)
	}

	doc := parsedCompose(t, path)
	svc, _ := doc["services"].(map[string]any)["web"].(map[string]any)
	env, ok := svc["environment"].([]any)
	if !ok {
		t.Fatalf("environment is %T, want the list the file was written with: %#v",
			svc["environment"], svc["environment"])
	}
	got := map[string]bool{}
	for _, e := range env {
		s, ok := e.(string)
		if !ok {
			t.Fatalf("a non-string entry appeared in the list: %#v", e)
		}
		if !strings.Contains(s, "=") {
			t.Errorf("%q is not KEY=value; the mapping form leaked into the list", s)
		}
		got[s] = true
	}
	if !got["REDIS_URL=redis://cache:6379"] {
		t.Errorf("the new variable is not in the list: %v", env)
	}
	if !got["DATABASE_URL=postgres://old"] || !got["NODE_ENV=production"] {
		t.Errorf("an existing variable was lost: %v", env)
	}
	if len(env) != 3 {
		t.Errorf("the list has %d entries, want 3: %v", len(env), env)
	}
}

// Setting a key the list already has REPLACES it rather than adding a second.
// Two entries for one name is valid YAML that compose resolves one way and a
// reader guesses at.
func TestSettingAnExistingListEntryReplacesIt(t *testing.T) {
	path := composeFile(t, `name: shop
services:
  web:
    build: .
    environment:
      - DATABASE_URL=postgres://old
`)

	if err := addEnvTo(path, "web", "DATABASE_URL", "postgres://new"); err != nil {
		t.Fatalf("addEnvTo: %v", err)
	}

	doc := parsedCompose(t, path)
	svc := doc["services"].(map[string]any)["web"].(map[string]any)
	env := svc["environment"].([]any)
	if len(env) != 1 {
		t.Fatalf("the list has %d entries, want one replaced: %v", len(env), env)
	}
	if env[0] != "postgres://new" && env[0] != "DATABASE_URL=postgres://new" {
		t.Errorf("entry = %v, want the new value", env[0])
	}
}

// A bare name in the list means "take it from the environment", and setting it
// replaces that rather than adding a second entry for the same name.
func TestABareNameInTheListIsReplaced(t *testing.T) {
	path := composeFile(t, `name: shop
services:
  web:
    build: .
    environment:
      - DATABASE_URL
`)

	if err := addEnvTo(path, "web", "DATABASE_URL", "postgres://new"); err != nil {
		t.Fatalf("addEnvTo: %v", err)
	}

	doc := parsedCompose(t, path)
	svc := doc["services"].(map[string]any)["web"].(map[string]any)
	env := svc["environment"].([]any)
	if len(env) != 1 {
		t.Errorf("the list has %d entries, want the bare name replaced: %v", len(env), env)
	}
}

// The mapping form still works, so the shape check did not cost the ordinary
// case.
func TestAMappingFormEnvironmentStillWorks(t *testing.T) {
	path := composeFile(t, `name: shop
services:
  web:
    build: .
    environment:
      NODE_ENV: production
`)

	if err := addEnvTo(path, "web", "REDIS_URL", "redis://cache:6379"); err != nil {
		t.Fatalf("addEnvTo: %v", err)
	}

	doc := parsedCompose(t, path)
	svc := doc["services"].(map[string]any)["web"].(map[string]any)
	env, ok := svc["environment"].(map[string]any)
	if !ok {
		t.Fatalf("environment is %T, want the mapping the file was written with", svc["environment"])
	}
	if env["REDIS_URL"] != "redis://cache:6379" || env["NODE_ENV"] != "production" {
		t.Errorf("environment = %v", env)
	}
}

// A service with no environment at all gains one, which is the case that
// created the mapping node in the first place.
func TestAServiceWithNoEnvironmentGainsAMapping(t *testing.T) {
	path := composeFile(t, `name: shop
services:
  web:
    build: .
`)

	if err := addEnvTo(path, "web", "NODE_ENV", "production"); err != nil {
		t.Fatalf("addEnvTo: %v", err)
	}

	doc := parsedCompose(t, path)
	svc := doc["services"].(map[string]any)["web"].(map[string]any)
	env, ok := svc["environment"].(map[string]any)
	if !ok || env["NODE_ENV"] != "production" {
		t.Errorf("environment = %#v", svc["environment"])
	}
}

// A shape pilot cannot edit is REFUSED, not written into. A scalar
// `environment: something` is a file somebody meant something by, and
// appending key/value pairs to it would destroy it.
func TestAnUneditableShapeIsRefused(t *testing.T) {
	path := composeFile(t, `name: shop
services:
  web:
    build: .
    environment: inherited
`)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	if err := addEnvTo(path, "web", "NODE_ENV", "production"); err == nil {
		t.Fatal("a scalar environment was written into")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Errorf("the refused edit still changed the file:\n%s", after)
	}
}
