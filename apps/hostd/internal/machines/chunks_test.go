package machines

import (
	"context"
	"net"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/vivek7405/pilots/hostd/internal/fc"
)

// The assertion the scrubbed environment exists for: a handler process sits
// beside a guest, parses bytes that guest's disk produced, and had this
// daemon's bucket credentials in its environment for no reason other than
// os.Environ() being one call.
func TestHandlerEnvCarriesNoPilotSecret(t *testing.T) {
	t.Setenv("PILOT_S3_ACCESS_KEY", "AKIAsecret")
	t.Setenv("PILOT_S3_SECRET_KEY", "supersecret")
	t.Setenv("PILOT_AGENT_TOKEN_SECRET", "fleet-wide")
	t.Setenv("PILOT_CORROSION_TOKEN", "gossip")
	t.Setenv("PATH", "/usr/bin:/bin")

	for _, kv := range HandlerEnv() {
		if strings.HasPrefix(kv, "PILOT_") {
			t.Errorf("a handler would inherit %q", kv)
		}
		if strings.Contains(kv, "secret") || strings.Contains(kv, "AKIA") {
			t.Errorf("a handler would inherit a credential: %q", kv)
		}
	}
}

// And it must still carry what a handler actually needs, or the hardening
// trades a credential for a machine that will not start.
func TestHandlerEnvKeepsWhatAHandlerNeeds(t *testing.T) {
	t.Setenv("PATH", "/usr/bin:/bin")
	t.Setenv("TMPDIR", "/var/tmp")

	env := HandlerEnv()
	for _, want := range []string{"PATH=/usr/bin:/bin", "TMPDIR=/var/tmp"} {
		found := false
		for _, kv := range env {
			if kv == want {
				found = true
			}
		}
		if !found {
			t.Errorf("%q is missing from the handler environment: %v", want, env)
		}
	}
}

// A host whose environment carries nothing still gets a usable PATH rather
// than none: a handler that cannot exec is a machine that will not boot.
func TestHandlerEnvFallsBackToAPath(t *testing.T) {
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		t.Setenv(k, "")
		_ = os.Unsetenv(k)
	}
	env := HandlerEnv()
	if len(env) == 0 || !strings.HasPrefix(env[0], "PATH=") {
		t.Errorf("an empty environment produced %v, want a fallback PATH", env)
	}
}

// The allowlist is exactly what the spawn already names, and nothing else. A
// fifth id here would be a build this machine has no reason to read.
func TestAllowedBuildsIsTheFourTheSpawnNames(t *testing.T) {
	mem := uuid.New()
	parent := uuid.New()
	template := uuid.New()

	cfg := fc.InstantConfig{}
	cfg.MemBuildID = mem
	cfg.MemParentBuildID = parent
	cfg.RootfsTemplateID = template

	got := allowedBuilds(cfg)
	if len(got) != 3 {
		t.Fatalf("allowedBuilds = %v, want the three ids that were set", got)
	}
	want := map[string]bool{mem.String(): true, parent.String(): true, template.String(): true}
	for _, id := range got {
		if !want[id] {
			t.Errorf("allowedBuilds included %q, which the spawn does not name", id)
		}
	}

	// A machine with no remote builds at all needs no socket, and an empty
	// allowlist is what tells the manager not to start one.
	if got := allowedBuilds(fc.InstantConfig{}); len(got) != 0 {
		t.Errorf("allowedBuilds on a local-only machine = %v, want none", got)
	}
}

type nopObjectStore struct{}

func (nopObjectStore) Get(context.Context, string) ([]byte, error) { return nil, nil }
func (nopObjectStore) GetRange(context.Context, string, int64, int64) ([]byte, error) {
	return nil, nil
}

// A create that retries against a re-derived template starts the same
// machine's chunk socket twice, at the same path. The second start must leave
// a socket there: closing the first server after the second had bound deleted
// it, and the retry's handlers then died on "no such file or directory".
func TestChunkServerRestartKeepsTheSocket(t *testing.T) {
	c := newChunkServers()
	dir := t.TempDir()
	allowed := []string{uuid.NewString()}

	first := c.start("m_1", dir, nopObjectStore{}, allowed)
	second := c.start("m_1", dir, nopObjectStore{}, allowed)
	t.Cleanup(func() { c.close("m_1") })
	if first == "" || second != first {
		t.Fatalf("start returned %q then %q, want the same socket path twice", first, second)
	}

	conn, err := net.Dial("unix", second)
	if err != nil {
		t.Fatalf("the restarted chunk socket is not there to dial: %v", err)
	}
	_ = conn.Close()
}
