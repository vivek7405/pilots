package build

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// The golden rootfs must carry the guest agent this tree builds.
//
// Nothing forced a rebuild when the agent changed, so the fleet ran a rootfs
// whose embedded agent was several commits old. It failed in a way that named
// none of that: hostd installed a machine's token, the stale agent had no
// reload path and kept the one it read at boot, and every call came back 401.
// A `systemctl restart` on the create path had been hiding it by making the
// old agent re-read at startup, so removing that restart -- a correct change
// worth 1.6 seconds on every create -- surfaced a rootfs that had been stale
// for some time.
//
// Skipped where the rootfs has not been built, which is every machine that has
// not run scripts/build-golden-rootfs.sh.
func TestGoldenRootfsCarriesThisAgent(t *testing.T) {
	root := repoRoot(t)
	rootfs := filepath.Join(root, "scripts", "rootfs", "golden.ext4")
	if _, err := os.Stat(rootfs); err != nil {
		t.Skip("no golden rootfs built here")
	}
	if _, err := exec.LookPath("debugfs"); err != nil {
		t.Skip("debugfs is not available to read the image")
	}

	dumped := filepath.Join(t.TempDir(), "agent")
	if out, err := exec.Command("debugfs", "-R",
		"dump /opt/pilot-agent/guest-agent "+dumped, rootfs).CombinedOutput(); err != nil {
		t.Skipf("could not read the agent out of the image: %v: %s", err, out)
	}

	built := filepath.Join(t.TempDir(), "guest-agent")
	// -trimpath must match scripts/build-golden-rootfs.sh exactly: without it
	// the binary embeds the builder's paths, and this comparison fails
	// whenever the image was packed by a different user.
	cmd := exec.Command("go", "build", "-trimpath", "-ldflags=-s -w", "-o", built,
		"./cmd/guest-agent")
	cmd.Dir = filepath.Join(root, "apps", "hostd")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building the agent: %v: %s", err, out)
	}

	if sum(t, dumped) != sum(t, built) {
		t.Error("the golden rootfs carries an agent this tree did not build.\n" +
			"Run scripts/build-golden-rootfs.sh and commit the new " +
			"scripts/rootfs/golden.ext4.sha256, or every machine created from it " +
			"runs a stale agent.")
	}
}

func sum(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func repoRoot(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		t.Skip("not in a git checkout")
	}
	return string(out[:len(out)-1])
}
