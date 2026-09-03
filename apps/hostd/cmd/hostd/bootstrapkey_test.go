package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vivek7405/pilots/hostd/internal/api"
	"github.com/vivek7405/pilots/hostd/internal/state"
)

// runBootstrapKeyCapturingStdout runs the subcommand and returns what it
// printed. The captured stdout is the contract: a script does
// KEY=$(hostd bootstrap-key), so anything else on that stream ends up in the
// key.
func runBootstrapKeyCapturingStdout(t *testing.T) string {
	t.Helper()

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	saved := os.Stdout
	os.Stdout = w
	runErr := runBootstrapKey()
	os.Stdout = saved
	w.Close()

	var sb strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := r.Read(buf)
		sb.Write(buf[:n])
		if err != nil {
			break
		}
	}
	r.Close()
	if runErr != nil {
		t.Fatalf("runBootstrapKey: %v", runErr)
	}
	return sb.String()
}

func TestBootstrapKeyMintsAnAdminKey(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "state.db")
	t.Setenv("PILOT_STATE_DSN", dsn)
	t.Setenv("PILOT_HOST_ID", "host-test")

	out := runBootstrapKeyCapturingStdout(t)

	// One line, and nothing else on stdout.
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 1 {
		t.Fatalf("stdout carried %d lines, want exactly the key: %q", len(lines), out)
	}
	key := lines[0]
	if !strings.HasPrefix(key, api.KeyPrefix) {
		t.Fatalf("printed %q, which is not a pilots key", key)
	}

	st, err := state.Open(dsn)
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	defer st.Close()

	sum := sha256.Sum256([]byte(key))
	rec, err := st.GetAPIKeyByHash(context.Background(), hex.EncodeToString(sum[:]))
	if err != nil {
		t.Fatalf("the printed key is not in the store: %v", err)
	}
	if rec.Scopes != api.ScopeAdmin {
		t.Errorf("scopes = %q, want admin: the first key has to be able to mint the rest", rec.Scopes)
	}
	if rec.OrgID != "ops" {
		t.Errorf("org = %q, want ops", rec.OrgID)
	}
}

func TestBootstrapKeyHonoursTheOrgOverride(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "state.db")
	t.Setenv("PILOT_STATE_DSN", dsn)
	t.Setenv("PILOT_HOST_ID", "host-test")
	t.Setenv(bootstrapOrgEnv, "org_custom")

	key := strings.TrimSpace(runBootstrapKeyCapturingStdout(t))

	st, err := state.Open(dsn)
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	defer st.Close()

	sum := sha256.Sum256([]byte(key))
	rec, err := st.GetAPIKeyByHash(context.Background(), hex.EncodeToString(sum[:]))
	if err != nil {
		t.Fatalf("GetAPIKeyByHash: %v", err)
	}
	if rec.OrgID != "org_custom" {
		t.Errorf("org = %q, want org_custom", rec.OrgID)
	}
}

// The systemd units read /etc/pilots/config as an EnvironmentFile; a root
// shell does not. Without loading it, a bootstrapped host running this by
// hand would open the wrong store and write a key nothing authenticates
// against.
func TestEnvironmentFileDoesNotOverrideTheEnvironment(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(path, []byte(
		"# a comment\nPILOT_FROM_FILE=yes\nPILOT_ALREADY_SET=\"from file\"\n\nnot-a-pair\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	t.Setenv("PILOT_ALREADY_SET", "from env")

	if err := loadEnvironmentFile(path); err != nil {
		t.Fatalf("loadEnvironmentFile: %v", err)
	}
	if got := os.Getenv("PILOT_FROM_FILE"); got != "yes" {
		t.Errorf("PILOT_FROM_FILE = %q, want yes", got)
	}
	if got := os.Getenv("PILOT_ALREADY_SET"); got != "from env" {
		t.Errorf("the file overrode an explicit environment: %q", got)
	}
	// A host that was never bootstrapped has no such file, and that is fine.
	if err := loadEnvironmentFile(filepath.Join(t.TempDir(), "absent")); err != nil {
		t.Errorf("a missing file is an error: %v", err)
	}
}
