package cli

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/vivek7405/pilots/cli/internal/config"
)

// A check is one line of the doctor's report.
type check struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
	Fix    string `json:"fix,omitempty"`
}

// newDoctorCmd is `fly doctor` for this setup, and the two failures it was
// written against are the ones that cost a day of misdiagnosis on
// 2026-09-09: a `pilot` alias in ~/.bashrc pointing at a deleted checkout,
// which shadowed the real binary in every interactive shell; and a local
// host whose PILOT_S3_ENDPOINT still named a network the laptop had left,
// which made every build fail at `packing rootfs` and every create answer
// `internal error`, nowhere near the cause. Each check names its fix.
func newDoctorCmd(env *Env, getenv config.Env) *cobra.Command {
	c := &cobra.Command{
		Use:   "doctor",
		Short: "check this machine's pilots setup and say what to fix",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			ctx, cancel := context.WithTimeout(c.Context(), 10*time.Second)
			defer cancel()
			checks := []check{
				checkShadowing(getenv),
				checkCredentials(getenv),
				checkFleet(ctx, env),
				checkKey(ctx, env),
				checkHarnesses(getenv),
			}
			if local := checkLocalHost(ctx); local != nil {
				checks = append(checks, *local)
			}
			if env.W.JSON {
				return env.W.JSONValue(checks)
			}
			failed := 0
			for _, ch := range checks {
				mark := "ok  "
				if !ch.OK {
					mark = "FAIL"
					failed++
				}
				env.W.Linef("%s  %-14s %s", mark, ch.Name, ch.Detail)
				if !ch.OK && ch.Fix != "" {
					env.W.Linef("      → %s", ch.Fix)
				}
			}
			if failed > 0 {
				return &ExitError{Code: 1}
			}
			return nil
		},
	}
	Describe(c, Doc{
		When: "First, when anything is strange: a command that does nothing, a\n" +
			"fleet that answers `internal error`, a build that dies at `packing\n" +
			"rootfs`. Each line that fails says what to change.",
		Examples: []string{"pilot doctor", "pilot doctor --json"},
	})
	return c
}

// checkShadowing looks for a `pilot` alias or function in the shell rc files
// that would win over this binary in an interactive shell -- the one class
// of problem `which pilot` cannot see, because aliases are not on PATH.
func checkShadowing(getenv config.Env) check {
	home := getenv("HOME")
	if home == "" {
		home, _ = os.UserHomeDir()
	}
	for _, rc := range []string{".bashrc", ".bash_profile", ".zshrc", ".profile", ".bash_aliases"} {
		f, err := os.Open(filepath.Join(home, rc))
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if strings.HasPrefix(line, "#") {
				continue
			}
			if strings.HasPrefix(line, "alias pilot=") || strings.HasPrefix(line, "pilot()") || strings.HasPrefix(line, "function pilot") {
				f.Close()
				return check{
					Name: "shell", OK: false,
					Detail: fmt.Sprintf("~/%s defines `pilot` itself: %s", rc, line),
					Fix:    fmt.Sprintf("remove that line from ~/%s; an alias wins over the binary on PATH in every interactive shell", rc),
				}
			}
		}
		f.Close()
	}
	self, _ := os.Executable()
	onPath, err := exec.LookPath("pilot")
	if err != nil {
		return check{Name: "shell", OK: false, Detail: "`pilot` is not on PATH", Fix: fmt.Sprintf("ln -sf %s ~/.local/bin/pilot", self)}
	}
	resolved, _ := filepath.EvalSymlinks(onPath)
	selfResolved, _ := filepath.EvalSymlinks(self)
	if resolved != "" && selfResolved != "" && resolved != selfResolved {
		return check{
			Name: "shell", OK: true,
			Detail: fmt.Sprintf("`pilot` on PATH is %s, this binary is %s", onPath, self),
		}
	}
	return check{Name: "shell", OK: true, Detail: "no alias shadows `pilot`; PATH resolves to this binary"}
}

func checkCredentials(getenv config.Env) check {
	path, _ := config.Path(getenv)
	creds, err := config.Load(getenv)
	if err != nil {
		return check{Name: "credentials", OK: false, Detail: err.Error(), Fix: "fix or remove " + path + ", then pilot login"}
	}
	if creds == nil {
		if getenv("PILOT_API_KEY") != "" {
			return check{Name: "credentials", OK: true, Detail: "no file; PILOT_API_KEY is set"}
		}
		return check{Name: "credentials", OK: false, Detail: "no " + path + " and no PILOT_API_KEY", Fix: "pilot login, or pilot login --token <key>"}
	}
	if st, err := os.Stat(path); err == nil && st.Mode().Perm()&0o077 != 0 {
		return check{Name: "credentials", OK: false, Detail: fmt.Sprintf("%s is mode %o, readable by others", path, st.Mode().Perm()), Fix: "chmod 600 " + path}
	}
	return check{Name: "credentials", OK: true, Detail: path}
}

func checkFleet(ctx context.Context, env *Env) check {
	u := strings.TrimRight(env.APIURL.Value, "/") + "/v1/health"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return check{Name: "fleet", OK: false, Detail: err.Error()}
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		fix := "is hostd running? on one box: sudo scripts/local-host.sh (docs/local.md)"
		if parsed, perr := url.Parse(env.APIURL.Value); perr == nil {
			if host, _, _ := net.SplitHostPort(parsed.Host); strings.HasSuffix(host, ".localhost") {
				fix += "; and " + host + " must resolve to 127.0.0.1 (systemd-resolved does this for *.localhost)"
			}
		}
		return check{Name: "fleet", OK: false, Detail: fmt.Sprintf("%s: %v", env.APIURL.Value, err), Fix: fix}
	}
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return check{Name: "fleet", OK: false, Detail: fmt.Sprintf("%s answered %d", u, res.StatusCode)}
	}
	return check{Name: "fleet", OK: true, Detail: fmt.Sprintf("%s (from %s)", env.APIURL.Value, env.APIURL.Source)}
}

func checkKey(ctx context.Context, env *Env) check {
	client, err := env.Client()
	if err != nil {
		return check{Name: "key", OK: false, Detail: "no API key", Fix: "pilot login, or set PILOT_API_KEY"}
	}
	who, err := client.Whoami(ctx)
	if err != nil {
		return check{Name: "key", OK: false, Detail: fmt.Sprintf("%s rejected the key from %s: %v", env.APIURL.Value, env.APIKey.Source, err), Fix: "pilot login again"}
	}
	return check{Name: "key", OK: true, Detail: fmt.Sprintf("org %s, scopes %s (from %s)", who.OrgID, joinScopes(who.Scopes), env.APIKey.Source)}
}

// checkLocalHost only runs where a single-box hostd config is readable. The
// S3 endpoint it holds is derived once and kept across runs, so on a laptop
// that changes networks it goes stale silently -- and the symptom surfaces
// minutes later and elsewhere.
func checkLocalHost(ctx context.Context) *check {
	raw, err := os.ReadFile("/etc/pilots/config")
	if err != nil {
		return nil
	}
	endpoint := ""
	for _, line := range strings.Split(string(raw), "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "PILOT_S3_ENDPOINT="); ok {
			endpoint = strings.Trim(v, `"'`)
		}
	}
	if endpoint == "" {
		return &check{Name: "local host", OK: true, Detail: "/etc/pilots/config has no S3 endpoint to check"}
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		return &check{Name: "local host", OK: false, Detail: "PILOT_S3_ENDPOINT is not a URL: " + endpoint}
	}
	dialer := net.Dialer{Timeout: 3 * time.Second}
	conn, err := dialer.DialContext(ctx, "tcp", u.Host)
	if err != nil {
		return &check{
			Name: "local host", OK: false,
			Detail: fmt.Sprintf("PILOT_S3_ENDPOINT %s is unreachable: %v", endpoint, err),
			Fix: "the address in /etc/pilots/config is stale (a laptop that changed networks) or minio is down: " +
				"point it at a stable local address such as a libvirt bridge, restart local-host.sh, and start local-s3.sh",
		}
	}
	conn.Close()
	return &check{Name: "local host", OK: true, Detail: "object store reachable at " + endpoint}
}
