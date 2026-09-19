package cli

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/spf13/cobra"

	"github.com/pilotsrun/pilots/cli/internal/out"
)

// Version is stamped at build time:
//
//	go build -ldflags "-X github.com/pilotsrun/pilots/cli/internal/cli.Version=v0.2.0"
//
// A binary built without it says "dev", and upgrade refuses to replace a dev
// build, because the person running one is the person changing it.
var Version = "dev"

const releasesAPI = "https://api.github.com/repos/pilotsrun/pilots/releases/latest"

type release struct {
	TagName string `json:"tag_name"`
	Assets  []struct {
		Name string `json:"name"`
		URL  string `json:"browser_download_url"`
	} `json:"assets"`
}

func newVersionCmd(env *Env) *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "print the CLI version",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			if env.W.JSON {
				return env.W.JSONValue(map[string]string{"version": Version, "os": runtime.GOOS, "arch": runtime.GOARCH})
			}
			env.W.Linef("pilot %s %s/%s", Version, runtime.GOOS, runtime.GOARCH)
			return nil
		},
	}
}

func newUpgradeCmd(env *Env) *cobra.Command {
	var checkOnly bool
	c := &cobra.Command{
		Use:   "upgrade",
		Short: "upgrade this binary to the latest release",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			req, err := http.NewRequestWithContext(c.Context(), http.MethodGet, releasesAPI, nil)
			if err != nil {
				return err
			}
			req.Header.Set("Accept", "application/vnd.github+json")
			res, err := http.DefaultClient.Do(req)
			if err != nil {
				return err
			}
			defer res.Body.Close()
			if res.StatusCode == http.StatusNotFound {
				return out.Failf("build from source: cd apps/pilot && go build ./cmd/pilot", "no release has been published yet")
			}
			if res.StatusCode != http.StatusOK {
				return fmt.Errorf("github answered %d for %s", res.StatusCode, releasesAPI)
			}
			var rel release
			if err := json.NewDecoder(res.Body).Decode(&rel); err != nil {
				return err
			}
			// The tag is `pilot-v0.2.0`; the binary is stamped with the
			// version alone, because the release workflow strips the prefix
			// before it passes -ldflags. Comparing the raw tag against the
			// stamp would never match, so `upgrade` would replace the newest
			// binary with itself and `--check` would advertise an upgrade
			// forever.
			latest := strings.TrimPrefix(rel.TagName, "pilot-")
			if env.W.JSON && checkOnly {
				return env.W.JSONValue(map[string]any{"current": Version, "latest": latest, "upgrade": latest != Version})
			}
			if latest == Version {
				env.W.Notef("pilot %s is the latest release", Version)
				return nil
			}
			if checkOnly {
				env.W.Notef("pilot %s is available (you have %s); run %s", latest, Version, upgradeCommand())
				return nil
			}
			if Version == "dev" {
				return out.Failf("a dev build is upgraded by rebuilding it", "this is a dev build, not a release")
			}

			want := fmt.Sprintf("pilot_%s_%s", runtime.GOOS, runtime.GOARCH)
			var assetURL, sumsURL string
			for _, a := range rel.Assets {
				switch a.Name {
				case want:
					assetURL = a.URL
				case checksumsAsset:
					sumsURL = a.URL
				}
			}
			if assetURL == "" {
				return out.Failf("build from source for this platform", "release %s carries no %s", latest, want)
			}
			self, err := os.Executable()
			if err != nil {
				return err
			}
			if self, err = filepath.EvalSymlinks(self); err != nil {
				return err
			}
			// Before anything is downloaded: a binary a package manager put
			// there is that manager's file. Renaming over it works today and
			// is undone, or reported as corruption, by the manager's next run.
			if name, cmd := managedBy(self); name != "" {
				return out.Failf(cmd, "this pilot was installed by %s, which owns %s", name, self)
			}
			// The release workflow writes checksums.txt in the same step as
			// the binaries, so a release without one was not cut by it.
			if sumsURL == "" {
				return out.Failf("re-run the installer: curl -fsSL https://pilots.run/install.sh | sh",
					"release %s carries no %s, so the download cannot be verified", latest, checksumsAsset)
			}
			wantSum, err := fetchChecksum(c.Context(), sumsURL, want)
			if err != nil {
				return err
			}
			if err := replaceBinary(c.Context(), self, assetURL, wantSum); err != nil {
				return err
			}
			env.W.Notef("upgraded pilot %s -> %s at %s", Version, latest, self)
			return nil
		},
	}
	c.Flags().BoolVar(&checkOnly, "check", false, "report whether a newer release exists without installing it")
	Describe(c, Doc{
		How: "The latest GitHub release is downloaded for this OS and architecture,\n" +
			"verified against the release's checksums.txt, and swapped in over the\n" +
			"running binary atomically, so an interrupted or tampered download\n" +
			"leaves the old one intact. A dev build is never replaced, and neither\n" +
			"is a binary npm or Homebrew installed: those upgrade through them.",
		Examples: []string{"pilot upgrade --check", "pilot upgrade"},
	})
	return c
}

// checksumsAsset is the third name in the contract the release workflow and
// the install script share with this file, beside the two binaries' names.
const checksumsAsset = "checksums.txt"

// managedBy names the package manager that owns the binary at path, and the
// command that upgrades it there, or two empty strings for a binary nobody
// owns (the install script's ~/.local/bin, a file somebody copied by hand).
// It reads the RESOLVED path: npm's and Homebrew's entries on PATH are both
// symlinks, and only what they point at says who put it there.
func managedBy(path string) (name, upgrade string) {
	slashed := filepath.ToSlash(path)
	switch {
	case strings.Contains(slashed, "/node_modules/"):
		return "npm", "npm install -g pilots@latest"
	case strings.Contains(slashed, "/Cellar/"):
		return "Homebrew", "brew upgrade pilot"
	}
	return "", ""
}

// upgradeCommand is what --check tells the reader to run. For a binary a
// package manager owns that is the manager's command: `pilot upgrade` there
// only refuses, so advising it costs a second run to learn the first answer.
func upgradeCommand() string {
	if self, err := os.Executable(); err == nil {
		if self, err = filepath.EvalSymlinks(self); err == nil {
			if _, cmd := managedBy(self); cmd != "" {
				return cmd
			}
		}
	}
	return "pilot upgrade"
}

// fetchChecksum reads the sha256 the release publishes for one asset.
func fetchChecksum(ctx context.Context, sumsURL, asset string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, sumsURL, nil)
	if err != nil {
		return "", err
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%s answered %d", checksumsAsset, res.StatusCode)
	}
	// A megabyte is thousands of lines; the real file is five.
	return checksumFor(io.LimitReader(res.Body, 1<<20), asset)
}

// checksumFor finds asset's line in sha256sum output: the digest, then two
// spaces (or a space and `*` in binary mode), then the name.
func checksumFor(r io.Reader, asset string) (string, error) {
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) != 2 || strings.TrimPrefix(fields[1], "*") != asset {
			continue
		}
		sum := strings.ToLower(fields[0])
		if raw, err := hex.DecodeString(sum); err != nil || len(raw) != sha256.Size {
			return "", fmt.Errorf("%s holds a malformed digest for %s", checksumsAsset, asset)
		}
		return sum, nil
	}
	if err := sc.Err(); err != nil {
		return "", err
	}
	return "", out.Failf("re-run the installer: curl -fsSL https://pilots.run/install.sh | sh",
		"%s publishes no digest for %s, so the download cannot be verified", checksumsAsset, asset)
}

// replaceBinary downloads beside the target, verifies what it wrote against
// wantSum, and only then renames over the target, so neither a half-written
// file nor a tampered one is ever the thing on PATH.
func replaceBinary(ctx context.Context, target, assetURL, wantSum string) error {
	if strings.HasSuffix(target, ".exe") {
		return out.Failf("replace the file by hand", "in-place upgrade is not supported on Windows")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, assetURL, nil)
	if err != nil {
		return err
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("download answered %d", res.StatusCode)
	}
	tmp, err := os.CreateTemp(filepath.Dir(target), ".pilot-upgrade-*")
	if err != nil {
		return out.Failf("the directory holding pilot is not writable; re-run with permission to it", "%v", err)
	}
	defer os.Remove(tmp.Name())
	// Hashed on the way to disk, so the digest is of the bytes that were
	// written and not of a second read somebody could race.
	hash := sha256.New()
	if _, err := io.Copy(io.MultiWriter(tmp, hash), res.Body); err != nil {
		tmp.Close()
		return err
	}
	if got := hex.EncodeToString(hash.Sum(nil)); got != wantSum {
		tmp.Close()
		return out.Failf("nothing was replaced; retry, and if it repeats report it",
			"checksum mismatch: %s says %s, the download is %s", checksumsAsset, wantSum, got)
	}
	if err := tmp.Chmod(0o755); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), target)
}
