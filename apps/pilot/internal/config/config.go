// Package config resolves the three things every command needs before it can
// talk to a fleet: which fleet, which key, and which org.
//
// The file format is the one packages/cli already writes, byte for byte, so a
// `pilot login` performed by either CLI is honoured by both. That is not
// politeness to the old CLI -- it is what lets the Go binary be swapped in and
// back out without an operator re-authenticating, which is the only way a
// replacement of a tool people already depend on can be reversible.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// DefaultAPIURL is the fleet a command talks to when nothing else names one.
// A single-box runbook and the e2e battery both use this address, so a CLI
// with no configuration at all still reaches a local host.
const DefaultAPIURL = "http://api.pilots.localhost:8080"

// Credentials is the on-disk file. The field names are load-bearing: they are
// the ones packages/cli/src/config.ts reads and writes.
//
// Secrets is carried through untouched rather than parsed. This CLI has no
// business reshaping a structure another tool owns, and dropping unknown keys
// on a rewrite would silently destroy an operator's secret values.
type Credentials struct {
	APIKey  string          `json:"api_key"`
	APIURL  string          `json:"api_url,omitempty"`
	Secrets json.RawMessage `json:"secrets,omitempty"`
}

// Env is the environment a resolution reads. It is an interface rather than
// os.Getenv so a test can resolve against a fixed environment without setting
// process-wide state, which would make the tests order-dependent.
type Env func(string) string

// OSEnv reads the real process environment.
func OSEnv(k string) string { return os.Getenv(k) }

// Path is where the credentials file lives: $XDG_CONFIG_HOME/pilots/credentials,
// falling back to ~/.config. Identical to credentialsPath() in the TS CLI.
func Path(env Env) (string, error) {
	base := env("XDG_CONFIG_HOME")
	if base == "" {
		home := env("HOME")
		if home == "" {
			var err error
			if home, err = os.UserHomeDir(); err != nil {
				return "", fmt.Errorf("no HOME and no home directory: %w", err)
			}
		}
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "pilots", "credentials"), nil
}

// Load reads the credentials file. A missing file is not an error: a fleet can
// be named entirely by environment, and reporting "no such file" for that case
// would send an operator looking for a file they never needed.
func Load(env Env) (*Credentials, error) {
	path, err := Path(env)
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var c Credentials
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("%s is not valid JSON: %w", path, err)
	}
	return &c, nil
}

// Save writes the credentials file atomically at mode 0600.
//
// Atomically because a torn write leaves an operator with neither the old key
// nor the new one, and 0600 because the file is a bearer token: anything that
// can read it can drive the whole fleet.
func Save(env Env, c *Credentials) error {
	path, err := Path(env)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(path), err)
	}
	body, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	body = append(body, '\n')

	tmp, err := os.CreateTemp(filepath.Dir(path), ".credentials-*")
	if err != nil {
		return fmt.Errorf("create a temporary file beside %s: %w", path, err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // a no-op once the rename succeeds

	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(body); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// Source says where a resolved value came from, so `pilot whoami` can show it.
// An operator debugging "why is it talking to the wrong fleet" needs the
// origin, not just the value.
type Source string

// Resolved is a value and where it came from.
type Resolved struct {
	Value  string
	Source Source
}

// Resolve works out the fleet, key and org for this invocation.
//
// Precedence, highest first: an explicit flag, then the environment, then the
// credentials file, then the built-in default. The flag wins because it is the
// most local statement of intent; the file loses because it is the most
// durable, and a durable value that overrode a deliberate one-off would be a
// trap. This is the same order the TS CLI documents.
func Resolve(env Env, flagURL, flagKey, flagOrg string) (url, key, org Resolved, err error) {
	creds, err := Load(env)
	if err != nil {
		// A malformed file must not stop a fully-specified invocation: a
		// command given both a URL and a key needs nothing from the file.
		if flagURL == "" || flagKey == "" {
			return url, key, org, err
		}
		creds = nil
	}
	path, perr := Path(env)
	if perr != nil {
		path = "the credentials file"
	}

	switch {
	case flagURL != "":
		url = Resolved{flagURL, "--api-url"}
	case env("PILOT_API_URL") != "":
		url = Resolved{env("PILOT_API_URL"), "PILOT_API_URL"}
	case creds != nil && creds.APIURL != "":
		url = Resolved{creds.APIURL, Source(path)}
	default:
		url = Resolved{DefaultAPIURL, "the default"}
	}

	switch {
	case flagKey != "":
		key = Resolved{flagKey, "--api-key"}
	case env("PILOT_API_KEY") != "":
		key = Resolved{env("PILOT_API_KEY"), "PILOT_API_KEY"}
	case creds != nil && creds.APIKey != "":
		key = Resolved{creds.APIKey, Source(path)}
	}

	switch {
	case flagOrg != "":
		org = Resolved{flagOrg, "--org"}
	case env("PILOT_ORG") != "":
		org = Resolved{env("PILOT_ORG"), "PILOT_ORG"}
	}

	return url, key, org, nil
}
