package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/spf13/cobra"

	"github.com/vivek7405/pilots/cli/internal/out"
)

// Version is stamped at build time:
//
//	go build -ldflags "-X github.com/vivek7405/pilots/cli/internal/cli.Version=v0.2.0"
//
// A binary built without it says "dev", and upgrade refuses to replace a dev
// build, because the person running one is the person changing it.
var Version = "dev"

const releasesAPI = "https://api.github.com/repos/vivek7405/pilots/releases/latest"

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
			latest := rel.TagName
			if env.W.JSON && checkOnly {
				return env.W.JSONValue(map[string]any{"current": Version, "latest": latest, "upgrade": latest != Version})
			}
			if latest == Version {
				env.W.Notef("pilot %s is the latest release", Version)
				return nil
			}
			if checkOnly {
				env.W.Notef("pilot %s is available (you have %s); run pilot upgrade", latest, Version)
				return nil
			}
			if Version == "dev" {
				return out.Failf("a dev build is upgraded by rebuilding it", "this is a dev build, not a release")
			}

			want := fmt.Sprintf("pilot_%s_%s", runtime.GOOS, runtime.GOARCH)
			var assetURL string
			for _, a := range rel.Assets {
				if a.Name == want {
					assetURL = a.URL
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
			if err := replaceBinary(c, self, assetURL); err != nil {
				return err
			}
			env.W.Notef("upgraded pilot %s -> %s at %s", Version, latest, self)
			return nil
		},
	}
	c.Flags().BoolVar(&checkOnly, "check", false, "report whether a newer release exists without installing it")
	Describe(c, Doc{
		How: "The latest GitHub release is downloaded for this OS and architecture\n" +
			"and swapped in over the running binary atomically, so an interrupted\n" +
			"upgrade leaves the old one intact. A dev build is never replaced.",
		Examples: []string{"pilot upgrade --check", "pilot upgrade"},
	})
	return c
}

// replaceBinary downloads beside the target and renames over it, so a
// half-written file is never the thing on PATH.
func replaceBinary(c *cobra.Command, target, assetURL string) error {
	req, err := http.NewRequestWithContext(c.Context(), http.MethodGet, assetURL, nil)
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
	if _, err := io.Copy(tmp, res.Body); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o755); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if strings.HasSuffix(target, ".exe") {
		return out.Failf("replace the file by hand", "in-place upgrade is not supported on Windows")
	}
	return os.Rename(tmp.Name(), target)
}
