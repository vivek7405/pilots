package cli

import (
	"archive/tar"
	"bytes"
	"io"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

func write(t *testing.T, dir, rel, content string) {
	t.Helper()
	full := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func names(t *testing.T, archive []byte) []string {
	t.Helper()
	var out []string
	tr := tar.NewReader(bytes.NewReader(archive))
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, hdr.Name)
	}
	sort.Strings(out)
	return out
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// .git is never part of a build context, and an excluded directory excludes
// everything under it -- that is what .dockerignore means, and getting it
// wrong ships node_modules to the builder.
func TestTarHonoursDockerignoreAndSkipsGit(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "Dockerfile", "FROM alpine")
	write(t, dir, "src/main.go", "package main")
	write(t, dir, "node_modules/x/index.js", "x")
	write(t, dir, ".git/HEAD", "ref")
	write(t, dir, "secret.env", "K=v")
	write(t, dir, ".dockerignore", "node_modules\n*.env\n")

	archive, err := tarDirectory(dir)
	if err != nil {
		t.Fatal(err)
	}
	got := names(t, archive)
	for _, want := range []string{"Dockerfile", "src/", "src/main.go", ".dockerignore"} {
		if !contains(got, want) {
			t.Errorf("%s is missing: %v", want, got)
		}
	}
	for _, banned := range []string{"node_modules/", "node_modules/x/index.js", ".git/", ".git/HEAD", "secret.env"} {
		if contains(got, banned) {
			t.Errorf("%s should have been excluded: %v", banned, got)
		}
	}
}

// A `!` rule re-includes, and the walk must not have pruned the directory it
// lives under.
func TestTarReincludesThroughANegation(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "build/keep.txt", "k")
	write(t, dir, "build/drop.txt", "d")
	write(t, dir, ".dockerignore", "build\n!build/keep.txt\n")

	archive, err := tarDirectory(dir)
	if err != nil {
		t.Fatal(err)
	}
	got := names(t, archive)
	if !contains(got, "build/keep.txt") {
		t.Errorf("keep.txt was not re-included: %v", got)
	}
	if contains(got, "build/drop.txt") {
		t.Errorf("drop.txt should still be excluded: %v", got)
	}
}

// The same tree packs to the same bytes, or the fleet's layer cache never
// hits. Ownership and ordering are the two things that would otherwise vary.
func TestTarIsDeterministic(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "b.txt", "b")
	write(t, dir, "a.txt", "a")
	first, err := tarDirectory(dir)
	if err != nil {
		t.Fatal(err)
	}
	second, err := tarDirectory(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Error("two packs of the same tree differ")
	}
	tr := tar.NewReader(bytes.NewReader(first))
	hdr, err := tr.Next()
	if err != nil {
		t.Fatal(err)
	}
	if hdr.Uid != 0 || hdr.Uname != "" {
		t.Errorf("ownership leaked into the archive: uid=%d uname=%q", hdr.Uid, hdr.Uname)
	}
}

// A symlink is kept as a symlink, not followed: following one
// node_modules/.bin tree is a copy of every target.
func TestTarKeepsSymlinks(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "real.txt", "r")
	if err := os.Symlink("real.txt", filepath.Join(dir, "link.txt")); err != nil {
		t.Skip("symlinks unsupported here")
	}
	archive, err := tarDirectory(dir)
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(bytes.NewReader(archive))
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			t.Fatal("link.txt not found")
		}
		if err != nil {
			t.Fatal(err)
		}
		if hdr.Name == "link.txt" {
			if hdr.Typeflag != tar.TypeSymlink || hdr.Linkname != "real.txt" {
				t.Errorf("link.txt was not kept as a symlink: type=%v link=%q", hdr.Typeflag, hdr.Linkname)
			}
			return
		}
	}
}
