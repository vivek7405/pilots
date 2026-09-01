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

	err := extractContext(r, dir, 1<<20)
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

	if err := extractContext(r, dir, 1<<20); err != nil {
		t.Fatalf("extractContext: %v", err)
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

	err := extractContext(r, dir, 1024)
	if err == nil || !strings.Contains(err.Error(), "larger than") {
		t.Fatalf("got %v, want a refusal naming the limit", err)
	}
}

func TestExtractContextWritesTheFiles(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "ctx")
	r := contextTar(t, map[string]string{
		"Dockerfile": "FROM alpine\n", "src/app.js": "console.log(1)\n",
	}, nil)

	if err := extractContext(r, dir, 1<<20); err != nil {
		t.Fatalf("extractContext: %v", err)
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
func TestSolveUsesTheTarExporterAndMachineReadableProgress(t *testing.T) {
	b := &Builder{opts: Options{BuildkitSock: "unix:///run/user/1000/buildkit/buildkitd.sock"}}
	args := b.solveArgs("/work/context", "/work/rootfs.tar", "")
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
	// No bucket configured: a cache export to nowhere fails the build rather
	// than being merely slower.
	if strings.Contains(joined, "export-cache") {
		t.Errorf("a cache export was requested with no bucket: %v", args)
	}
}

func TestSolveArgsWireUpTheSharedCache(t *testing.T) {
	b := &Builder{opts: Options{
		CacheBucket: "pilots", CacheEndpoint: "https://ep", CacheRegion: "auto",
		CacheAccessKey: "ak", CacheSecretKey: "sk",
	}}
	joined := strings.Join(b.solveArgs("/ctx", "/out.tar", "df-abc"), " ")

	base := "type=s3,bucket=pilots,endpoint_url=https://ep,region=auto,name=df-abc," +
		"use_path_style=true,access_key_id=ak,secret_access_key=sk"
	for _, want := range []string{
		"--export-cache " + base + ",mode=max",
		"--import-cache " + base,
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in %s", want, joined)
		}
	}
}

// The cache backend runs inside buildkitd, which has no credentials of its
// own and no way to acquire any: it is rootless, runs as another user, and
// knows nothing of hostd's configuration. Without these attributes it reaches
// for the EC2 metadata service and fails the build on a context deadline that
// names IMDS rather than the cache.
func TestTheCacheCarriesItsOwnCredentials(t *testing.T) {
	b := &Builder{opts: Options{
		CacheBucket: "pilots", CacheEndpoint: "https://ep", CacheRegion: "auto",
		CacheAccessKey: "ak", CacheSecretKey: "sk",
	}}
	joined := strings.Join(b.solveArgs("/ctx", "/out.tar", "df-abc"), " ")

	for _, want := range []string{"access_key_id=ak", "secret_access_key=sk", "use_path_style=true"} {
		if !strings.Contains(joined, want) {
			t.Errorf("the cache spec is missing %q: %s", want, joined)
		}
	}
}

// No credentials configured means no credential attributes, rather than empty
// ones: an empty access_key_id is a different request from an absent one, and
// the daemon's own chain is the right fallback on a host that has one.
func TestTheCacheOmitsAbsentCredentials(t *testing.T) {
	b := &Builder{opts: Options{
		CacheBucket: "pilots", CacheEndpoint: "https://ep", CacheRegion: "auto",
	}}
	joined := strings.Join(b.solveArgs("/ctx", "/out.tar", "df-abc"), " ")

	if strings.Contains(joined, "access_key_id") {
		t.Errorf("an empty credential was sent: %s", joined)
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
	_, err := b.Build(context.Background(), "bld-1",
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
