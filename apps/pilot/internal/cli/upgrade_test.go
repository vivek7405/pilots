package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func sumOf(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func serve(t *testing.T, body []byte) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.Write(body) }))
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestReplaceBinaryVerifiesBeforeItRenames(t *testing.T) {
	fresh := []byte("the new binary")
	url := serve(t, fresh)

	t.Run("a matching digest replaces the target", func(t *testing.T) {
		target := filepath.Join(t.TempDir(), "pilot")
		if err := os.WriteFile(target, []byte("old"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := replaceBinary(context.Background(), target, url, sumOf(fresh)); err != nil {
			t.Fatal(err)
		}
		got, _ := os.ReadFile(target)
		if string(got) != string(fresh) {
			t.Fatalf("target holds %q", got)
		}
		if info, _ := os.Stat(target); info.Mode().Perm() != 0o755 {
			t.Fatalf("target mode is %v", info.Mode().Perm())
		}
	})

	t.Run("a mismatch leaves the old binary and no temp file", func(t *testing.T) {
		dir := t.TempDir()
		target := filepath.Join(dir, "pilot")
		if err := os.WriteFile(target, []byte("old"), 0o755); err != nil {
			t.Fatal(err)
		}
		err := replaceBinary(context.Background(), target, url, sumOf([]byte("something else")))
		if err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
			t.Fatalf("err = %v", err)
		}
		got, _ := os.ReadFile(target)
		if string(got) != "old" {
			t.Fatalf("target was replaced with %q", got)
		}
		entries, _ := os.ReadDir(dir)
		if len(entries) != 1 {
			t.Fatalf("the directory holds %d entries, want only the old binary", len(entries))
		}
	})
}

func TestChecksumFor(t *testing.T) {
	a, b := sumOf([]byte("a")), sumOf([]byte("b"))
	sums := a + "  pilot_linux_amd64\n" + strings.ToUpper(b) + " *pilot_darwin_arm64\n"

	if got, err := checksumFor(strings.NewReader(sums), "pilot_linux_amd64"); err != nil || got != a {
		t.Fatalf("text mode: %q, %v", got, err)
	}
	// Binary mode's `*`, and a digest somebody upper-cased, are the same file.
	if got, err := checksumFor(strings.NewReader(sums), "pilot_darwin_arm64"); err != nil || got != b {
		t.Fatalf("binary mode: %q, %v", got, err)
	}
	// A name that is only a prefix of a published one is not that asset.
	if _, err := checksumFor(strings.NewReader(sums), "pilot_linux"); err == nil {
		t.Fatal("a prefix matched")
	}
	if _, err := checksumFor(strings.NewReader("nothex  pilot_linux_amd64\n"), "pilot_linux_amd64"); err == nil {
		t.Fatal("a malformed digest was accepted")
	}
}

func TestManagedBy(t *testing.T) {
	for path, want := range map[string]string{
		"/home/u/.local/bin/pilot": "",
		"/usr/local/bin/pilot":     "",
		"/usr/lib/node_modules/pilots/node_modules/@pilots/cli-linux-x64/bin/pilot":              "npm",
		"/home/u/.nvm/versions/node/v24.0.0/lib/node_modules/@pilots/cli-darwin-arm64/bin/pilot": "npm",
		"/opt/homebrew/Cellar/pilot/0.2.0/bin/pilot":                                             "Homebrew",
		"/home/linuxbrew/.linuxbrew/Cellar/pilot/0.2.0/bin/pilot":                                "Homebrew",
	} {
		name, cmd := managedBy(path)
		if name != want {
			t.Errorf("managedBy(%q) = %q, want %q", path, name, want)
		}
		if (name == "") != (cmd == "") {
			t.Errorf("managedBy(%q) names %q but the command is %q", path, name, cmd)
		}
	}
}
