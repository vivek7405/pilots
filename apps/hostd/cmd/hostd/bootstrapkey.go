package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/vivek7405/pilots/hostd/internal/api"
	"github.com/vivek7405/pilots/hostd/internal/config"
	"github.com/vivek7405/pilots/hostd/internal/state"
)

// BootstrapKeyCommand mints the first admin key on a fleet and prints it.
//
// Every other key comes from POST /v1/api-keys, which needs an admin key to
// call -- so a fleet with no keys at all cannot mint its first one over the
// API. This is the way in, and it is deliberately only reachable from a root
// shell on a host: it writes the api_keys row directly, with no credential to
// present, which is exactly what nothing on the network may do.
const BootstrapKeyCommand = "bootstrap-key"

// bootstrapKeyTimeout bounds the whole run. On a fleet, openState waits for
// corrosion to apply its schema, which is the slow part.
const bootstrapKeyTimeout = 3 * time.Minute

// bootstrapOrgEnv names the org the first key belongs to. The ops org by
// default: an admin key is the fleet operator's, not a customer's.
const bootstrapOrgEnv = "PILOT_BOOTSTRAP_ORG"

func runBootstrapKey() error {
	// The systemd units read /etc/pilots/config as an EnvironmentFile; a root
	// shell does not. Without this, a bootstrapped host running this command
	// by hand would load defaults, open the wrong store, and write a key
	// nothing authenticates against.
	if err := loadEnvironmentFile("/etc/pilots/config"); err != nil {
		return err
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), bootstrapKeyTimeout)
	defer cancel()

	store, _, err := openState(ctx, cfg)
	if err != nil {
		return err
	}
	defer store.Close()

	key, hash, err := api.MintKey(rand.Reader)
	if err != nil {
		return err
	}
	org := os.Getenv(bootstrapOrgEnv)
	if org == "" {
		org = "ops"
	}
	if err := store.PutAPIKey(ctx, &state.APIKey{
		Hash: hash, OrgID: org, Scopes: api.ScopeAdmin,
		CreatedAt: time.Now().Unix(),
	}); err != nil {
		return err
	}

	// The plaintext, and nothing else, on stdout: a script captures this with
	// KEY=$(hostd bootstrap-key), and any other line would end up in the key.
	fmt.Println(key)
	return nil
}

// loadEnvironmentFile applies a systemd EnvironmentFile to this process.
//
// Only for names not already set, so an explicit environment still wins -- a
// test, or an operator overriding one value for one run, must not be silently
// overwritten by the file.
func loadEnvironmentFile(path string) error {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // not a bootstrapped host; the environment is all there is
		}
		return err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		name = strings.TrimSpace(name)
		if _, set := os.LookupEnv(name); set {
			continue
		}
		if err := os.Setenv(name, strings.Trim(strings.TrimSpace(value), `"`)); err != nil {
			return err
		}
	}
	return sc.Err()
}

// dispatchBootstrapKey runs the subcommand if that is what was asked for.
func dispatchBootstrapKey() bool {
	if len(os.Args) < 2 || os.Args[1] != BootstrapKeyCommand {
		return false
	}
	if err := runBootstrapKey(); err != nil {
		// stderr, so a caller capturing stdout gets the key or nothing.
		fmt.Fprintf(os.Stderr, "bootstrap-key: %v\n", err)
		os.Exit(1)
	}
	return true
}
