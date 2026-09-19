package cli

import (
	"testing"

	pilots "github.com/pilotsrun/pilots/sdks/go"
)

// A secret the plan references comes from PILOT_SECRET_<NAME> when that is
// set: a CI runner has no credentials file and never ran `pilot secrets set`.
// The environment overrides the file, and a name is upper-cased with every
// character outside [A-Z0-9] turned into an underscore.
func TestSecretsComeFromTheEnvironmentWhenSet(t *testing.T) {
	if got, want := secretEnvVar("database_url"), "PILOT_SECRET_DATABASE_URL"; got != want {
		t.Fatalf("secretEnvVar = %q, want %q", got, want)
	}
	if got, want := secretEnvVar("db-password.v2"), "PILOT_SECRET_DB_PASSWORD_V2"; got != want {
		t.Fatalf("secretEnvVar = %q, want %q", got, want)
	}

	plan := pilots.ComposePlan{Steps: []pilots.ComposeStep{{
		SecretRefs: map[string]string{"DATABASE_URL": "database_url", "POSTGRES_PASSWORD": "postgres_password"},
	}}}
	env := map[string]string{"PILOT_SECRET_POSTGRES_PASSWORD": "from-env", "PILOT_SECRET_DATABASE_URL": "env-wins"}
	getenv := func(k string) string { return env[k] }

	got := withEnvSecrets(map[string]string{"database_url": "from-file"}, plan, getenv)
	if got["postgres_password"] != "from-env" {
		t.Fatalf("a secret only the environment holds was not read: %v", got)
	}
	if got["database_url"] != "env-wins" {
		t.Fatalf("the environment did not override the file: %v", got)
	}
	if withEnvSecrets(nil, plan, func(string) string { return "" }) != nil {
		t.Fatal("an empty environment invented a store")
	}
}
