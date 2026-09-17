package build

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeMetadata writes a --metadata-file carrying the image config inline
// under containerimage.config.
//
// NO SHIPPED BUILDKIT WRITES THIS. 0.32 publishes only the config's digest,
// and a tar-only build writes no metadata file at all. This fixture describes
// the forward-compatible branch readImageConfig keeps for a version that
// might publish the config inline; it was once believed to be what buildkit
// did, which is how a broken reader stayed green for the life of the feature.
// The real shape is exercised by
// TestTheBaseConfigIsRecoveredFromWhatBuildkitReallyWrites.
func writeMetadata(t *testing.T, cfg map[string]any) string {
	t.Helper()
	doc, err := json.Marshal(map[string]any{"config": cfg})
	if err != nil {
		t.Fatal(err)
	}
	meta := map[string]any{
		"containerimage.config.digest": "sha256:" + strings.Repeat("a", 64),
		imageConfigKey:                 base64.StdEncoding.EncodeToString(doc),
	}
	raw, err := json.Marshal(meta)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "metadata.json")
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestReadImageConfigDecodesAnInlineConfigIfOneIsEverPublished(t *testing.T) {
	path := writeMetadata(t, map[string]any{
		"Env":          []string{"PATH=/usr/local/bin:/usr/bin", "PGDATA=/var/lib/postgresql/data"},
		"Cmd":          []string{"postgres"},
		"Entrypoint":   []string{"docker-entrypoint.sh"},
		"WorkingDir":   "/app",
		"User":         "postgres",
		"ExposedPorts": map[string]any{"5432/tcp": map[string]any{}},
	})

	cfg, err := readImageConfig(path)
	if err != nil {
		t.Fatalf("readImageConfig: %v", err)
	}
	if cfg == nil {
		t.Fatal("readImageConfig returned no config for metadata that carries one")
	}
	if len(cfg.Cmd) != 1 || cfg.Cmd[0] != "postgres" {
		t.Errorf("Cmd = %v, want [postgres]", cfg.Cmd)
	}
	if cfg.WorkingDir != "/app" || cfg.User != "postgres" {
		t.Errorf("WorkingDir/User = %q/%q, want /app/postgres", cfg.WorkingDir, cfg.User)
	}
	if _, ok := cfg.ExposedPorts["5432/tcp"]; !ok {
		t.Errorf("ExposedPorts = %v, want 5432/tcp", cfg.ExposedPorts)
	}
}

// Which keys an exporter publishes is a property of the buildkit the builder
// image pins. A build that produced a correct filesystem must not fail because
// one moved, so an absent key is nil and no error.
func TestReadImageConfigIsAbsentNotAnError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metadata.json")
	if err := os.WriteFile(path, []byte(`{"containerimage.config.digest":"sha256:x"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := readImageConfig(path)
	if err != nil || cfg != nil {
		t.Errorf("readImageConfig = %v, %v; a missing key is nil and no error", cfg, err)
	}
}

func TestReadImageConfigReportsUnreadableMetadata(t *testing.T) {
	if _, err := readImageConfig(filepath.Join(t.TempDir(), "nope.json")); err == nil {
		t.Error("a missing metadata file returned no error")
	}
	path := filepath.Join(t.TempDir(), "metadata.json")
	if err := os.WriteFile(path, []byte(`{"containerimage.config":"not base64!!"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readImageConfig(path); err == nil {
		t.Error("an undecodable config returned no error")
	}
}

// The whole point of A1: `FROM postgres:17` with nothing else declared has to
// come out with the image's own command, environment, workdir, user and port.
func TestMergeImageConfigFillsAnEmptySpec(t *testing.T) {
	spec := StartSpec{FromDockerfileOnly: true}.MergeImageConfig(&ImageConfig{
		Entrypoint:   []string{"docker-entrypoint.sh"},
		Cmd:          []string{"postgres"},
		Env:          []string{"PGDATA=/var/lib/postgresql/data"},
		WorkingDir:   "/var/lib/postgresql",
		User:         "postgres",
		ExposedPorts: map[string]struct{}{"5432/tcp": {}},
	})

	if len(spec.Entrypoint) != 1 || spec.Entrypoint[0] != "docker-entrypoint.sh" {
		t.Errorf("Entrypoint = %v, want the image's", spec.Entrypoint)
	}
	if len(spec.Cmd) != 1 || spec.Cmd[0] != "postgres" {
		t.Errorf("Cmd = %v, want the image's", spec.Cmd)
	}
	if spec.WorkDir != "/var/lib/postgresql" || spec.User != "postgres" {
		t.Errorf("WorkDir/User = %q/%q", spec.WorkDir, spec.User)
	}
	if spec.Port != 5432 {
		t.Errorf("Port = %d, want 5432", spec.Port)
	}
	if spec.Env["PGDATA"] != "/var/lib/postgresql/data" {
		t.Errorf("Env = %v, want PGDATA from the image", spec.Env)
	}
	if spec.FromDockerfileOnly {
		t.Error("from_dockerfile_only stayed true after the image config was merged in")
	}
	if spec.Empty() {
		t.Error("the spec is still empty, so the machine has nothing to start")
	}
}

// Docker's rule, and the one that is easy to get wrong: a Dockerfile that
// declares its own ENTRYPOINT and no CMD does NOT inherit the image's CMD,
// because those arguments were written for a different program. Inheriting
// them here would run `postgres` as an argument to someone's entrypoint.
func TestMergeImageConfigDropsTheImageCmdUnderANewEntrypoint(t *testing.T) {
	spec := StartSpec{
		Entrypoint:         []string{"/app/start.sh"},
		FromDockerfileOnly: true,
	}.MergeImageConfig(&ImageConfig{
		Entrypoint: []string{"docker-entrypoint.sh"},
		Cmd:        []string{"postgres"},
	})

	if len(spec.Cmd) != 0 {
		t.Errorf("Cmd = %v; a new entrypoint discards the image's arguments", spec.Cmd)
	}
	if spec.Entrypoint[0] != "/app/start.sh" {
		t.Errorf("Entrypoint = %v; the Dockerfile's own must win", spec.Entrypoint)
	}
}

// The mirror image: CMD alone in the Dockerfile is arguments TO the image's
// entrypoint, which is what `FROM postgres:17` plus a tuning CMD means.
func TestMergeImageConfigKeepsTheImageEntrypointUnderANewCmd(t *testing.T) {
	spec := StartSpec{
		Cmd:                []string{"-c", "shared_buffers=256MB"},
		FromDockerfileOnly: true,
	}.MergeImageConfig(&ImageConfig{
		Entrypoint: []string{"docker-entrypoint.sh"},
		Cmd:        []string{"postgres"},
	})

	if len(spec.Entrypoint) != 1 || spec.Entrypoint[0] != "docker-entrypoint.sh" {
		t.Errorf("Entrypoint = %v, want the image's", spec.Entrypoint)
	}
	if len(spec.Cmd) != 2 || spec.Cmd[0] != "-c" {
		t.Errorf("Cmd = %v, want the Dockerfile's arguments", spec.Cmd)
	}
}

// Env merges key by key, because a Dockerfile's ENV is written as an addition
// to the base image's environment rather than a replacement for it. The
// Dockerfile still wins every key it names.
func TestMergeImageConfigMergesEnvAndLetsTheDockerfileWin(t *testing.T) {
	spec := StartSpec{
		Env:                map[string]string{"NODE_ENV": "production", "PATH": "/app/bin"},
		FromDockerfileOnly: true,
	}.MergeImageConfig(&ImageConfig{
		Env: []string{"PATH=/usr/local/bin", "LANG=C.UTF-8", "malformed", "=novalue"},
	})

	if spec.Env["PATH"] != "/app/bin" {
		t.Errorf("PATH = %q; the Dockerfile's own ENV must win", spec.Env["PATH"])
	}
	if spec.Env["LANG"] != "C.UTF-8" {
		t.Errorf("LANG = %q; a key only the image sets must survive", spec.Env["LANG"])
	}
	if spec.Env["NODE_ENV"] != "production" {
		t.Errorf("NODE_ENV = %q; the Dockerfile's own key was lost", spec.Env["NODE_ENV"])
	}
	if _, ok := spec.Env[""]; ok {
		t.Error("an empty key from a malformed entry reached the environment")
	}
}

// A map has no order, so "the first exposed port" would differ between two
// runs of the same build and the machine's published port would move.
func TestMergeImageConfigTakesTheLowestExposedTCPPort(t *testing.T) {
	spec := StartSpec{FromDockerfileOnly: true}.MergeImageConfig(&ImageConfig{
		Cmd: []string{"serve"},
		ExposedPorts: map[string]struct{}{
			"8080/tcp": {}, "5432/tcp": {}, "53/udp": {}, "junk": {},
		},
	})
	if spec.Port != 5432 {
		t.Errorf("Port = %d, want 5432: the lowest TCP port, deterministically", spec.Port)
	}

	udpOnly := StartSpec{FromDockerfileOnly: true}.MergeImageConfig(&ImageConfig{
		Cmd:          []string{"serve"},
		ExposedPorts: map[string]struct{}{"53/udp": {}},
	})
	if udpOnly.Port != 0 {
		t.Errorf("Port = %d for a UDP-only image; the router speaks TCP", udpOnly.Port)
	}
}

// A build whose daemon published nothing keeps exactly the behaviour every
// build had before this existed.
func TestMergeImageConfigWithNoConfigChangesNothing(t *testing.T) {
	before := StartSpec{Cmd: []string{"node", "server.js"}, FromDockerfileOnly: true}
	after := before.MergeImageConfig(nil)
	if !after.FromDockerfileOnly || len(after.Cmd) != 2 {
		t.Errorf("a nil image config changed the spec: %+v", after)
	}
}

func TestSolveArgsAsksForTheImageConfig(t *testing.T) {
	b := &Builder{opts: Options{BuildctlBin: "buildctl"}}
	joined := strings.Join(b.solveArgs(testBuilderAddr, "/ctx", "/out.tar", "", "", "/work/metadata.json"), " ")
	if !strings.Contains(joined, "--metadata-file /work/metadata.json") {
		t.Errorf("solveArgs did not ask for the image config: %s", joined)
	}

	without := strings.Join(b.solveArgs(testBuilderAddr, "/ctx", "/out.tar", "", "", ""), " ")
	if strings.Contains(without, "--metadata-file") {
		t.Errorf("solveArgs passed an empty --metadata-file: %s", without)
	}
}

// writeOCILayout writes a real OCI image layout: index.json naming a manifest
// blob, the manifest naming a config blob, and the config carrying cfg. This
// is the shape a `--output type=oci,tar=false` export actually leaves on disk.
func writeOCILayout(t *testing.T, dir string, cfg map[string]any) {
	t.Helper()
	blobs := filepath.Join(dir, "blobs", "sha256")
	if err := os.MkdirAll(blobs, 0o755); err != nil {
		t.Fatal(err)
	}
	put := func(name string, v any) string {
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(raw)
		hex := hex.EncodeToString(sum[:])
		if err := os.WriteFile(filepath.Join(blobs, hex), raw, 0o644); err != nil {
			t.Fatal(err)
		}
		return "sha256:" + hex
	}
	cfgDigest := put("config", map[string]any{"config": cfg})
	manDigest := put("manifest", map[string]any{
		"mediaType": "application/vnd.oci.image.manifest.v1+json",
		"config":    map[string]any{"digest": cfgDigest},
		"layers":    []any{},
	})
	idx, err := json.Marshal(map[string]any{
		"manifests": []any{map[string]any{
			"mediaType": "application/vnd.oci.image.manifest.v1+json",
			"digest":    manDigest,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "index.json"), idx, 0o644); err != nil {
		t.Fatal(err)
	}
}

// The regression that mattered: on the buildkit this product actually pins,
// the metadata file carries only the config's DIGEST, never the config. Every
// build therefore lost its base image's start command, so `FROM alpine` with
// no CMD of its own reported "this image declares no CMD or ENTRYPOINT" even
// though alpine declares /bin/sh. The old test passed throughout, because its
// fixture invented a containerimage.config key no buildkit version writes.
func TestTheBaseConfigIsRecoveredFromWhatBuildkitReallyWrites(t *testing.T) {
	dir := t.TempDir()
	meta := filepath.Join(dir, "metadata.json")

	// Exactly what buildkit 0.32 publishes: the digest, and no config.
	raw, err := json.Marshal(map[string]any{
		"containerimage.config.digest": "sha256:" + strings.Repeat("b", 64),
		"containerimage.digest":        "sha256:" + strings.Repeat("c", 64),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(meta, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	writeOCILayout(t, ociLayoutDir(meta), map[string]any{
		"Cmd": []string{"/bin/sh"},
		"Env": []string{"PATH=/usr/local/sbin:/usr/local/bin"},
	})

	cfg, err := readImageConfig(meta)
	if err != nil {
		t.Fatalf("readImageConfig: %v", err)
	}
	if cfg == nil {
		t.Fatal("no config recovered; the base image's CMD is lost and the build " +
			"will refuse an image that declares one")
	}
	if len(cfg.Cmd) != 1 || cfg.Cmd[0] != "/bin/sh" {
		t.Errorf("Cmd = %v, want [/bin/sh]", cfg.Cmd)
	}
	if len(cfg.Env) != 1 {
		t.Errorf("Env = %v, want the base image's PATH", cfg.Env)
	}
}

// A layout that is an index pointing at an index pointing at a manifest, which
// is what a multi-platform export leaves.
func TestTheBaseConfigIsRecoveredThroughANestedIndex(t *testing.T) {
	dir := t.TempDir()
	meta := filepath.Join(dir, "metadata.json")
	if err := os.WriteFile(meta, []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	layout := ociLayoutDir(meta)
	writeOCILayout(t, layout, map[string]any{"Cmd": []string{"postgres"}})

	// Wrap the existing index in one more level.
	inner, err := os.ReadFile(filepath.Join(layout, "index.json"))
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(inner)
	h := hex.EncodeToString(sum[:])
	if err := os.WriteFile(filepath.Join(layout, "blobs", "sha256", h), inner, 0o644); err != nil {
		t.Fatal(err)
	}
	outer, err := json.Marshal(map[string]any{
		"manifests": []any{map[string]any{"digest": "sha256:" + h}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(layout, "index.json"), outer, 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := readImageConfig(meta)
	if err != nil || cfg == nil {
		t.Fatalf("readImageConfig through a nested index: cfg=%v err=%v", cfg, err)
	}
	if len(cfg.Cmd) != 1 || cfg.Cmd[0] != "postgres" {
		t.Errorf("Cmd = %v, want [postgres]", cfg.Cmd)
	}
}

// A digest that tries to climb out of the blob store is refused rather than
// read. The layout is written by a build, but a build runs tenant input.
func TestAnEscapingDigestIsRefused(t *testing.T) {
	dir := t.TempDir()
	layout := filepath.Join(dir, "x.oci")
	if err := os.MkdirAll(layout, 0o755); err != nil {
		t.Fatal(err)
	}
	idx, err := json.Marshal(map[string]any{
		"manifests": []any{map[string]any{"digest": "sha256:../../../../etc/passwd"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(layout, "index.json"), idx, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := configFromOCILayout(layout); err == nil {
		t.Fatal("a digest containing path separators was accepted")
	}
}
