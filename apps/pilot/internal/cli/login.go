package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/vivek7405/pilots/cli/internal/config"
	"github.com/vivek7405/pilots/cli/internal/out"
)

const (
	defaultGitHubURL    = "https://github.com"
	defaultDashboardURL = "https://pilots.run"
	deviceScope         = "read:user user:email"
)

func newLoginCmd(env *Env, getenv config.Env) *cobra.Command {
	var (
		token string
		org   string
	)
	c := &cobra.Command{
		Use:   "login",
		Short: "authenticate with GitHub and store a pilots API key",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			existing, _ := config.Load(getenv)
			creds := &config.Credentials{APIURL: env.APIURL.Value}
			if existing != nil {
				creds.Secrets = existing.Secrets
			}

			switch {
			case token != "":
				// Deliberately not validated against anything: a --token login
				// is how a fleet with no GitHub App gets its first key, and
				// that fleet may not be reachable from here yet.
				creds.APIKey, creds.OrgID = token, org
			case getenv("PILOT_GITHUB_CLIENT_ID") == "":
				// No App to talk to, so ask for a key rather than fail: the
				// single-box runbook mints one with `hostd bootstrap-key`.
				key, err := promptSecret("API key: ", "or run pilot login --token <key>")
				if err != nil {
					return err
				}
				creds.APIKey = key
			default:
				ghToken, err := deviceFlow(c.Context(), getenv, env.W)
				if err != nil {
					return err
				}
				res, err := exchangeToken(c.Context(), getenv, ghToken)
				if err != nil {
					return err
				}
				creds.APIKey, creds.OrgID = res.APIKey, res.OrgID
			}

			if err := config.Save(getenv, creds); err != nil {
				return err
			}
			path, _ := config.Path(getenv)
			if env.W.JSON {
				return env.W.JSONValue(map[string]any{"org_id": creds.OrgID, "api_url": creds.APIURL, "path": path})
			}
			if creds.OrgID != "" {
				env.W.Notef("logged in as %s", creds.OrgID)
			}
			env.W.Notef("API key stored in %s", path)
			return nil
		},
	}
	c.Flags().StringVar(&token, "token", "", "skip GitHub and store this API key directly (headless)")
	c.Flags().StringVar(&org, "org", "", "the org id to record alongside a --token key")
	Describe(c, Doc{
		How: "Three ways in, one file out. Interactively it is the GitHub device\n" +
			"flow: open the URL, enter the code, and the dashboard exchanges the\n" +
			"GitHub token for a pilots key. Headless, --token stores a key you\n" +
			"already have. On a fleet with no GitHub App (a single box) it asks\n" +
			"for the key `hostd bootstrap-key` printed.\n\n" +
			"The file is ~/.config/pilots/credentials, mode 0600. Secrets already\n" +
			"stored there are kept.",
		Examples: []string{
			"pilot login",
			"pilot login --token pilot_... --org acme",
			"# a different fleet",
			"pilot login --api-url http://api.pilots.localhost:8080",
		},
		Related: []string{
			"pilot whoami   what is stored, and where each value came from",
			"pilot logout   remove the file",
		},
	})
	return c
}

func newLogoutCmd(env *Env, getenv config.Env) *cobra.Command {
	c := &cobra.Command{
		Use:   "logout",
		Short: "remove the stored credentials",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			path, err := config.Path(getenv)
			if err != nil {
				return err
			}
			err = os.Remove(path)
			removed := err == nil
			if err != nil && !os.IsNotExist(err) {
				return err
			}
			if env.W.JSON {
				return env.W.JSONValue(map[string]any{"removed": removed, "path": path})
			}
			if removed {
				env.W.Notef("removed %s", path)
			} else {
				env.W.Notef("not logged in")
			}
			return nil
		},
	}
	Describe(c, Doc{
		Warning: "The file holds the API key AND every secret stored with\n" +
			"`pilot secrets`. Both go. Back the secrets up first if they exist\n" +
			"nowhere else.",
		Examples: []string{"pilot logout"},
	})
	return c
}

// promptSecret reads one line from the terminal with echo off.
func promptSecret(message, hint string) (string, error) {
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		return "", out.Failf(hint, "stdin is not a terminal, so there is nothing to prompt on")
	}
	fmt.Fprint(os.Stderr, message)
	raw, err := term.ReadPassword(fd)
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return "", err
	}
	v := strings.TrimSpace(string(raw))
	if v == "" {
		return "", out.Failf(hint, "no value entered")
	}
	return v, nil
}

type deviceCode struct {
	DeviceCode      string `json:"device_code"`
	UserCode        string `json:"user_code"`
	VerificationURI string `json:"verification_uri"`
	ExpiresIn       int    `json:"expires_in"`
	Interval        int    `json:"interval"`
}

type accessToken struct {
	AccessToken      string `json:"access_token"`
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
	Interval         int    `json:"interval"`
}

// deviceFlow is GitHub's device authorization grant: ask for a code, show
// it, poll until the person has entered it. The same flow the TS CLI runs,
// including a second code when the first expires unentered.
func deviceFlow(ctx context.Context, getenv config.Env, w *out.Writer) (string, error) {
	base := strings.TrimRight(firstNonEmpty(getenv("PILOT_GITHUB_URL"), defaultGitHubURL), "/")
	clientID := getenv("PILOT_GITHUB_CLIENT_ID")
	for attempt := 0; attempt < 2; attempt++ {
		var code deviceCode
		if err := postForm(ctx, base+"/login/device/code", url.Values{"client_id": {clientID}, "scope": {deviceScope}}, &code); err != nil {
			return "", err
		}
		w.Notef("Open %s and enter code %s", code.VerificationURI, code.UserCode)
		interval := max(1, code.Interval)
		expired := false
		for poll := 0; poll < 200 && !expired; poll++ {
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(time.Duration(interval) * time.Second):
			}
			var tok accessToken
			if err := postForm(ctx, base+"/login/oauth/access_token", url.Values{
				"client_id": {clientID}, "device_code": {code.DeviceCode},
				"grant_type": {"urn:ietf:params:oauth:grant-type:device_code"},
			}, &tok); err != nil {
				return "", err
			}
			if tok.AccessToken != "" {
				return tok.AccessToken, nil
			}
			switch tok.Error {
			case "authorization_pending":
			case "slow_down":
				if tok.Interval > 0 {
					interval = tok.Interval
				} else {
					interval += 5
				}
			case "expired_token":
				expired = true
			case "access_denied":
				return "", out.Failf("run pilot login again", "authorization denied")
			default:
				return "", fmt.Errorf("github: %s: %s", tok.Error, tok.ErrorDescription)
			}
		}
		if !expired {
			return "", out.Failf("run pilot login again", "gave up waiting for GitHub")
		}
	}
	return "", out.Failf("run pilot login again and enter the code within the time GitHub shows", "the device code expired twice")
}

type exchangeResult struct {
	APIKey string   `json:"api_key"`
	OrgID  string   `json:"org_id"`
	Scopes []string `json:"scopes"`
}

// exchangeToken trades a GitHub token for a pilots key at the dashboard,
// which is the one place that can map a GitHub identity to an org.
func exchangeToken(ctx context.Context, getenv config.Env, ghToken string) (*exchangeResult, error) {
	base := strings.TrimRight(firstNonEmpty(getenv("PILOT_DASHBOARD_URL"), defaultDashboardURL), "/")
	body, _ := json.Marshal(map[string]string{"github_access_token": ghToken})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/api/cli/exchange", strings.NewReader(string(body)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	text, _ := io.ReadAll(res.Body)
	if res.StatusCode >= 300 {
		return nil, fmt.Errorf("%s/api/cli/exchange failed with %d: %s", base, res.StatusCode, strings.TrimSpace(string(text)))
	}
	var r exchangeResult
	if err := json.Unmarshal(text, &r); err != nil || r.APIKey == "" || r.OrgID == "" {
		return nil, fmt.Errorf("the exchange response was missing api_key or org_id: %s", strings.TrimSpace(string(text)))
	}
	return &r, nil
}

func postForm(ctx context.Context, u string, form url.Values, into any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	text, _ := io.ReadAll(res.Body)
	if res.StatusCode >= 300 {
		return fmt.Errorf("github: %s answered %d: %s", u, res.StatusCode, strings.TrimSpace(string(text)))
	}
	if err := json.Unmarshal(text, into); err != nil {
		return fmt.Errorf("github: %s returned non-JSON: %s", u, strings.TrimSpace(string(text)))
	}
	return nil
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// readLine reads one line from stdin, for a prompt that is not a secret.
func readLine() (string, error) {
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && line == "" {
		return "", err
	}
	return strings.TrimSpace(line), nil
}
