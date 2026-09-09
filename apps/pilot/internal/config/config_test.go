package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// envFrom builds an Env from a map so a test never touches the process
// environment, which would make these order-dependent.
func envFrom(m map[string]string) Env {
	return func(k string) string { return m[k] }
}

func TestPathFollowsXDGThenHome(t *testing.T) {
	got, err := Path(envFrom(map[string]string{"XDG_CONFIG_HOME": "/xdg"}))
	if err != nil {
		t.Fatal(err)
	}
	if want := "/xdg/pilots/credentials"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}

	got, err = Path(envFrom(map[string]string{"HOME": "/home/someone"}))
	if err != nil {
		t.Fatal(err)
	}
	if want := "/home/someone/.config/pilots/credentials"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestAMissingFileIsNotAnError(t *testing.T) {
	env := envFrom(map[string]string{"XDG_CONFIG_HOME": t.TempDir()})
	creds, err := Load(env)
	if err != nil {
		t.Fatalf("a missing credentials file must not error: %v", err)
	}
	if creds != nil {
		t.Errorf("got %+v, want nil", creds)
	}
}

// The whole point of matching the TS CLI's format: a file written by one is
// read by the other. This pins the wire names rather than the Go field names.
func TestReadsTheFileTheTypeScriptCLIWrites(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "pilots"), 0o700); err != nil {
		t.Fatal(err)
	}
	written := `{
  "api_key": "pilot_abc",
  "api_url": "http://api.example:8080",
  "org_id": "acme",
  "secrets": { "mini-crm": { "db-password": "s3cret" } }
}`
	if err := os.WriteFile(filepath.Join(dir, "pilots", "credentials"), []byte(written), 0o600); err != nil {
		t.Fatal(err)
	}

	creds, err := Load(envFrom(map[string]string{"XDG_CONFIG_HOME": dir}))
	if err != nil {
		t.Fatal(err)
	}
	if creds.APIKey != "pilot_abc" {
		t.Errorf("api_key: got %q", creds.APIKey)
	}
	if creds.APIURL != "http://api.example:8080" {
		t.Errorf("api_url: got %q", creds.APIURL)
	}
	// login writes org_id; a struct without the field would drop it on save.
	if creds.OrgID != "acme" {
		t.Errorf("org_id: got %q", creds.OrgID)
	}
	if len(creds.Secrets) == 0 {
		t.Error("secrets were dropped")
	}
}

// A rewrite must not destroy the secrets another tool owns, so a load/save
// round trip has to carry the whole structure through untouched.
func TestSaveKeepsSecretsItDoesNotUnderstand(t *testing.T) {
	dir := t.TempDir()
	env := envFrom(map[string]string{"XDG_CONFIG_HOME": dir})
	if err := os.MkdirAll(filepath.Join(dir, "pilots"), 0o700); err != nil {
		t.Fatal(err)
	}
	original := `{"api_key":"k1","secrets":{"app":{"a":"1"}}}`
	path := filepath.Join(dir, "pilots", "credentials")
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}

	creds, err := Load(env)
	if err != nil {
		t.Fatal(err)
	}
	creds.APIKey = "k2"
	if err := Save(env, creds); err != nil {
		t.Fatal(err)
	}

	var back map[string]any
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if back["api_key"] != "k2" {
		t.Errorf("api_key: got %v", back["api_key"])
	}
	secrets, ok := back["secrets"].(map[string]any)
	if !ok {
		t.Fatalf("secrets were lost on save: %v", back["secrets"])
	}
	if _, ok := secrets["app"]; !ok {
		t.Errorf("the app's secrets were lost: %v", secrets)
	}
}

func TestSaveIsPrivateToTheUser(t *testing.T) {
	dir := t.TempDir()
	env := envFrom(map[string]string{"XDG_CONFIG_HOME": dir})
	if err := Save(env, &Credentials{APIKey: "k"}); err != nil {
		t.Fatal(err)
	}
	path, _ := Path(env)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	// The file is a bearer token; group or world readability is a leak.
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("mode is %o, want 600", perm)
	}
}

func TestPrecedenceFlagBeatsEnvBeatsFileBeatsDefault(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "pilots"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "pilots", "credentials"),
		[]byte(`{"api_key":"from-file","api_url":"http://file:8080"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	base := map[string]string{"XDG_CONFIG_HOME": dir}

	// The file alone.
	url, key, _, err := Resolve(envFrom(base), "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if url.Value != "http://file:8080" || key.Value != "from-file" {
		t.Errorf("file: got url=%q key=%q", url.Value, key.Value)
	}

	// The environment beats the file.
	withEnv := map[string]string{"XDG_CONFIG_HOME": dir, "PILOT_API_URL": "http://env:8080", "PILOT_API_KEY": "from-env"}
	url, key, _, err = Resolve(envFrom(withEnv), "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if url.Value != "http://env:8080" || key.Value != "from-env" {
		t.Errorf("env: got url=%q key=%q", url.Value, key.Value)
	}
	if url.Source != "PILOT_API_URL" {
		t.Errorf("source: got %q", url.Source)
	}

	// PILOT_API is the name the e2e battery and the runbook use.
	url, _, _, err = Resolve(envFrom(map[string]string{"XDG_CONFIG_HOME": dir, "PILOT_API": "http://alias:8080"}), "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if url.Value != "http://alias:8080" || url.Source != "PILOT_API" {
		t.Errorf("PILOT_API: got %q from %q", url.Value, url.Source)
	}

	// The flag beats the environment.
	url, key, _, err = Resolve(envFrom(withEnv), "http://flag:8080", "from-flag", "")
	if err != nil {
		t.Fatal(err)
	}
	if url.Value != "http://flag:8080" || key.Value != "from-flag" {
		t.Errorf("flag: got url=%q key=%q", url.Value, key.Value)
	}

	// Nothing at all still names a fleet, so a local box works unconfigured.
	url, _, _, err = Resolve(envFrom(map[string]string{"XDG_CONFIG_HOME": t.TempDir()}), "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if url.Value != DefaultAPIURL || url.Source != "the default" {
		t.Errorf("default: got %q from %q", url.Value, url.Source)
	}
}

// A broken file must not stop an invocation that needed nothing from it.
func TestAFullySpecifiedInvocationSurvivesABrokenFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "pilots"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "pilots", "credentials"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	env := envFrom(map[string]string{"XDG_CONFIG_HOME": dir})

	if _, _, _, err := Resolve(env, "http://flag:8080", "from-flag", ""); err != nil {
		t.Errorf("a flagged url and key need nothing from the file: %v", err)
	}
	// But a command that DOES need the file is told the file is the problem.
	if _, _, _, err := Resolve(env, "", "", ""); err == nil {
		t.Error("a malformed file must be reported when it is actually needed")
	}
}
