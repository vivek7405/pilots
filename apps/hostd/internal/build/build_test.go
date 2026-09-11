package build

import (
	"archive/tar"
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vivek7405/pilots/hostd/internal/api"
)

func contextTar(t *testing.T, entries map[string]string, extra []tar.Header) *bytes.Reader {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for name, body := range entries {
		if err := tw.WriteHeader(&tar.Header{
			Name: name, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(body)),
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	for _, h := range extra {
		h := h
		if err := tw.WriteHeader(&h); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return bytes.NewReader(buf.Bytes())
}

// The context is an archive uploaded by whoever holds an API key. Every entry
// is checked against the destination root, because `../../root/.ssh/
// authorized_keys` inside a tar is the oldest trick there is and the builder
// runs on a box that is also serving other tenants' machines.
func TestExtractContextRefusesPathsOutsideItself(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "ctx")
	r := contextTar(t, map[string]string{
		"Dockerfile":        "FROM scratch\n",
		"../../etc/escaped": "pwned",
		"nested/../ok.txt":  "fine",
	}, nil)

	err := ExtractContext(r, dir, 1<<20)
	if err == nil || !strings.Contains(err.Error(), "outside") {
		t.Fatalf("got %v, want a refusal naming the escaping path", err)
	}
}

// A symlink in an untrusted archive is how a LATER entry in the same archive
// writes outside the context. There is no legitimate use for one in a build
// context, so they are dropped rather than recreated.
func TestExtractContextDropsSymlinks(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "ctx")
	r := contextTar(t, map[string]string{"Dockerfile": "FROM scratch\n"},
		[]tar.Header{{Name: "escape", Typeflag: tar.TypeSymlink, Linkname: "/etc", Mode: 0o777}})

	if err := ExtractContext(r, dir, 1<<20); err != nil {
		t.Fatalf("ExtractContext: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(dir, "escape")); err == nil {
		t.Fatal("a symlink from the context archive was recreated on disk")
	}
}

// The byte budget is what stops a POST filling the host's disk.
func TestExtractContextEnforcesTheSizeLimit(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "ctx")
	r := contextTar(t, map[string]string{
		"Dockerfile": "FROM scratch\n",
		"big.bin":    strings.Repeat("x", 4096),
	}, nil)

	err := ExtractContext(r, dir, 1024)
	if err == nil || !strings.Contains(err.Error(), "larger than") {
		t.Fatalf("got %v, want a refusal naming the limit", err)
	}
}

func TestExtractContextWritesTheFiles(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "ctx")
	r := contextTar(t, map[string]string{
		"Dockerfile": "FROM alpine\n", "src/app.js": "console.log(1)\n",
	}, nil)

	if err := ExtractContext(r, dir, 1<<20); err != nil {
		t.Fatalf("ExtractContext: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "src", "app.js"))
	if err != nil || string(got) != "console.log(1)\n" {
		t.Fatalf("read back %q, %v", got, err)
	}
}

// The exporter decision, asserted rather than left in a comment: the tar
// exporter emits the FLATTENED filesystem, which is what an ext4 image needs.
// The oci exporter emits a layered image tarball, and unpacking those layers
// means reimplementing whiteout and diff ordering to arrive at bytes the tar
// exporter already produced.
// testBuilderAddr stands in for a builder machine's tap address. The daemon is
// never on this host, so every solve is addressed over TCP to a guest.
const testBuilderAddr = "tcp://10.11.0.2:1234"

// The daemon buildctl dials lives INSIDE the org's builder machine. Nothing
// here may reach for a host socket: that is the whole difference between
// running a customer Dockerfile behind KVM and running it beside other
// tenants' machines.
func TestSolveDialsTheBuilderMachineAndNeverAHostSocket(t *testing.T) {
	b := &Builder{opts: Options{}}
	args := b.solveArgs(testBuilderAddr, "/work/context", "/work/rootfs.tar", "", "")

	if args[0] != "--addr" || args[1] != testBuilderAddr {
		t.Errorf("solve did not dial the builder machine: %v", args[:2])
	}
	if strings.Contains(strings.Join(args, " "), "unix://") {
		t.Errorf("solve reached for a host socket: %v", args)
	}
}

func TestSolveUsesTheTarExporterAndMachineReadableProgress(t *testing.T) {
	b := &Builder{opts: Options{}}
	args := b.solveArgs(testBuilderAddr, "/work/context", "/work/rootfs.tar", "", "")
	joined := strings.Join(args, " ")

	if !strings.Contains(joined, "--output type=tar,dest=/work/rootfs.tar") {
		t.Errorf("expected the tar exporter: %v", args)
	}
	if strings.Contains(joined, "type=oci") || strings.Contains(joined, "type=docker") {
		t.Errorf("an image exporter was requested: %v", args)
	}
	if !strings.Contains(joined, "--progress rawjson") {
		t.Errorf("expected machine-readable progress: %v", args)
	}
	if !strings.Contains(joined, "--frontend dockerfile.v0") {
		t.Errorf("expected the dockerfile frontend: %v", args)
	}
	// No cache directory: nothing is exported rather than exported nowhere.
	if strings.Contains(joined, "export-cache") {
		t.Errorf("a cache export was requested with no bucket: %v", args)
	}
}

func TestSolveArgsWireUpTheOrgsCacheDirectory(t *testing.T) {
	b := &Builder{opts: Options{CacheDir: "/var/cache/pilots/build-cache"}}
	dir := b.cacheDir("org_1", "df-abc")
	joined := strings.Join(b.solveArgs(testBuilderAddr, "/ctx", "/out.tar", dir, ""), " ")

	for _, want := range []string{
		"--export-cache type=local,dest=" + dir + ",mode=max",
		"--import-cache type=local,src=" + dir,
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in %s", want, joined)
		}
	}
	// The old cache exported straight to a bucket from inside the daemon.
	// Nothing may do that any more: the daemon runs in a guest the org
	// controls.
	if strings.Contains(joined, "type=s3") {
		t.Errorf("the daemon was pointed at object storage: %s", joined)
	}
}

// The whole cross-tenant argument rests on this. A daemon inside an org's
// machine that held bucket credentials could write a cache manifest under any
// key, and the next org whose Dockerfile hashed the same would import it. So
// no credential may appear in what buildctl is given, ever.
func TestNoCredentialEverReachesTheDaemon(t *testing.T) {
	b := &Builder{opts: Options{CacheDir: "/var/cache/pilots/build-cache"}}
	joined := strings.Join(
		b.solveArgs(testBuilderAddr, "/ctx", "/out.tar", b.cacheDir("org_1", "df-abc"), "/seed"), " ")

	for _, forbidden := range []string{
		"access_key_id", "secret_access_key", "AWS_", "endpoint_url", "bucket=",
	} {
		if strings.Contains(joined, forbidden) {
			t.Errorf("a credential or bucket reached the daemon (%q): %s", forbidden, joined)
		}
	}
}

// An org's cache is its own directory. Two orgs building the SAME Dockerfile
// get the same cache name and must still not share a directory, or one org's
// layers become the other's.
func TestTwoOrgsNeverShareACacheDirectory(t *testing.T) {
	b := &Builder{opts: Options{CacheDir: "/cache"}}
	if a, other := b.cacheDir("org_1", "df-abc"), b.cacheDir("org_2", "df-abc"); a == other {
		t.Fatalf("org_1 and org_2 share %q for the same Dockerfile", a)
	}
	// A build with no org gets no cache at all rather than a pooled one: an
	// admin-key build must not be able to write something a tenant reads.
	if got := b.cacheDir("", "df-abc"); got != "" {
		t.Errorf("an org-less build was given a cache directory: %q", got)
	}
	// And with no cache root configured, nothing is exported anywhere.
	none := &Builder{opts: Options{}}
	if got := none.cacheDir("org_1", "df-abc"); got != "" {
		t.Errorf("a cache directory appeared with no cache root: %q", got)
	}
}

// The shared seed is imported read-only and never exported to. Only hostd
// writes it, from a Dockerfile that is a constant in this package, which is
// what makes it safe for every org to build on.
func TestTheSharedSeedIsImportOnly(t *testing.T) {
	b := &Builder{opts: Options{CacheDir: "/cache"}}
	joined := strings.Join(
		b.solveArgs(testBuilderAddr, "/ctx", "/out.tar", b.cacheDir("org_1", "df-abc"), "/cache/shared/node-base"), " ")

	if !strings.Contains(joined, "--import-cache type=local,src=/cache/shared/node-base") {
		t.Errorf("the shared seed was not imported: %s", joined)
	}
	if strings.Contains(joined, "--export-cache type=local,dest=/cache/shared/node-base") {
		t.Errorf("a build exported into the shared seed: %s", joined)
	}
}

// The cache key is the Dockerfile's own content: two deploys of one Dockerfile
// share a cache wherever they land, and two different ones never collide.
func TestCacheNameFollowsTheDockerfile(t *testing.T) {
	a, bdir := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(a, "Dockerfile"), []byte("FROM alpine\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bdir, "Dockerfile"), []byte("FROM alpine\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if cacheNameFor(a) != cacheNameFor(bdir) {
		t.Error("the same Dockerfile in two places got two cache keys")
	}
	if err := os.WriteFile(filepath.Join(bdir, "Dockerfile"), []byte("FROM node\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if cacheNameFor(a) == cacheNameFor(bdir) {
		t.Error("two different Dockerfiles share a cache key")
	}
	if cacheNameFor(t.TempDir()) != "" {
		t.Error("a context with no Dockerfile produced a cache key")
	}
}

// A build with no Dockerfile fails at the edge with a message that says so,
// rather than inside BuildKit with one that does not.
func TestBuildRejectsAContextWithNoDockerfile(t *testing.T) {
	b := &Builder{
		opts: Options{WorkRoot: t.TempDir(), MaxContextBytes: 1 << 20},
		logs: newLogStore(4), sem: make(chan struct{}, 1), run: execRunner,
	}
	var lines []api.BuildLogLine
	_, err := b.Build(context.Background(), "bld-1", "org-1",
		contextTar(t, map[string]string{"app.js": "x"}, nil),
		func(l api.BuildLogLine) { lines = append(lines, l) })

	if err == nil || !strings.Contains(err.Error(), "Dockerfile") {
		t.Fatalf("got %v, want a refusal naming the missing Dockerfile", err)
	}
	// The failure has to be IN the stream, not only in the return value: a
	// client reading the stream is the one that has to act on it.
	var sawError bool
	for _, l := range lines {
		if l.Error != "" {
			sawError = true
		}
	}
	if !sawError {
		t.Fatalf("the failure never reached the log stream: %+v", lines)
	}
}
