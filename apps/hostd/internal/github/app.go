package github

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// App is the GitHub App's identity: its numeric id and its signing key.
type App struct {
	ID         int64
	PrivateKey *rsa.PrivateKey
	// Secret is the webhook shared secret.
	Secret string

	HTTP *http.Client
}

// LoadApp reads the app's credentials.
//
// The private key is a file rather than an environment variable because it is
// multi-line PEM, and every layer between here and a systemd unit mangles
// those differently.
func LoadApp(id int64, keyPath, secret string) (*App, error) {
	if id == 0 || keyPath == "" {
		return nil, nil // not configured; push-to-deploy is simply off
	}
	raw, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, fmt.Errorf("github: read app key: %w", err)
	}
	block, _ := pem.Decode(raw)
	if block == nil {
		return nil, fmt.Errorf("github: %s is not PEM", keyPath)
	}
	key, err := x509.ParsePKCS1PrivateKey(block.Bytes)
	if err != nil {
		parsed, err8 := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err8 != nil {
			return nil, fmt.Errorf("github: app key is neither PKCS1 nor PKCS8: %w", err)
		}
		var ok bool
		if key, ok = parsed.(*rsa.PrivateKey); !ok {
			return nil, fmt.Errorf("github: app key is not RSA")
		}
	}
	return &App{ID: id, PrivateKey: key, Secret: secret,
		HTTP: &http.Client{Timeout: 60 * time.Second}}, nil
}

// jwt mints the short-lived assertion that proves this is the App.
//
// Ten minutes is GitHub's ceiling and there is no reason to ask for less: the
// token it buys lasts an hour, and a clock a few seconds fast on either side
// would reject a tighter window.
func (a *App) jwt() (string, error) {
	now := time.Now()
	header := map[string]string{"alg": "RS256", "typ": "JWT"}
	claims := map[string]any{
		"iat": now.Add(-30 * time.Second).Unix(), // tolerate a slow clock here
		"exp": now.Add(9 * time.Minute).Unix(),
		"iss": a.ID,
	}
	enc := func(v any) (string, error) {
		raw, err := json.Marshal(v)
		if err != nil {
			return "", err
		}
		return base64.RawURLEncoding.EncodeToString(raw), nil
	}
	h, err := enc(header)
	if err != nil {
		return "", err
	}
	c, err := enc(claims)
	if err != nil {
		return "", err
	}
	signing := h + "." + c
	sig, err := signRS256(a.PrivateKey, signing)
	if err != nil {
		return "", err
	}
	return signing + "." + sig, nil
}

// InstallationToken mints a token scoped to one installation.
//
// Minted per job and never cached across builds. They last an hour, and a
// cached one is a credential sitting in memory long after the work it was for
// finished -- for an installation whose permissions may since have been
// narrowed or revoked.
func (a *App) InstallationToken(ctx context.Context, installationID int64) (string, error) {
	assertion, err := a.jwt()
	if err != nil {
		return "", err
	}
	url := fmt.Sprintf("https://api.github.com/app/installations/%d/access_tokens", installationID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+assertion)
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := a.HTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return "", fmt.Errorf("github: installation token: %s: %s",
			resp.Status, readSnippet(resp.Body))
	}
	var out struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	return out.Token, nil
}

// Tarball streams a repository at a ref into a build context.
//
// GitHub answers with a 302 to a codeload URL that is valid for about five
// minutes on a private repository, so the redirect is followed immediately and
// the body streamed straight through rather than staged. The single wrapping
// directory is stripped on the way past -- see StripRoot.
func (a *App) Tarball(ctx context.Context, token, repo, ref string, w io.Writer) error {
	url := fmt.Sprintf("https://api.github.com/repos/%s/tarball/%s", repo, ref)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := a.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("github: tarball %s@%s: %s: %s", repo, ref,
			resp.Status, readSnippet(resp.Body))
	}

	gz, err := gzip.NewReader(resp.Body)
	if err != nil {
		return fmt.Errorf("github: tarball is not gzip: %w", err)
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	tw := tar.NewWriter(w)
	defer tw.Close()

	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return tw.Close()
		}
		if err != nil {
			return err
		}
		name, ok := StripRoot(hdr.Name)
		if !ok {
			continue // the wrapper directory entry itself
		}
		hdr.Name = name
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if hdr.Typeflag == tar.TypeReg {
			if _, err := io.Copy(tw, tr); err != nil {
				return err
			}
		}
	}
}

// Comment posts or updates the preview comment on a pull request.
//
// Upsert rather than append: a pull request that is pushed to ten times should
// carry one comment naming the current preview, not ten naming previews that
// no longer exist.
func (a *App) Comment(ctx context.Context, token, repo string, number int, marker, body string) error {
	existing, err := a.findComment(ctx, token, repo, number, marker)
	if err != nil {
		return err
	}
	payload, _ := json.Marshal(map[string]string{"body": marker + "\n" + body})

	url := fmt.Sprintf("https://api.github.com/repos/%s/issues/%d/comments", repo, number)
	method := http.MethodPost
	if existing != 0 {
		url = fmt.Sprintf("https://api.github.com/repos/%s/issues/comments/%d", repo, existing)
		method = http.MethodPatch
	}
	req, err := http.NewRequestWithContext(ctx, method, url, strings.NewReader(string(payload)))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := a.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("github: comment on %s#%d: %s", repo, number, resp.Status)
	}
	return nil
}

func (a *App) findComment(ctx context.Context, token, repo string, number int, marker string) (int64, error) {
	url := fmt.Sprintf("https://api.github.com/repos/%s/issues/%d/comments?per_page=100", repo, number)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := a.HTTP.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return 0, nil // not fatal: post a new one rather than fail the preview
	}
	var comments []struct {
		ID   int64  `json:"id"`
		Body string `json:"body"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&comments); err != nil {
		return 0, nil
	}
	for _, c := range comments {
		if strings.Contains(c.Body, marker) {
			return c.ID, nil
		}
	}
	return 0, nil
}

func readSnippet(r io.Reader) string {
	buf := make([]byte, 400)
	n, _ := r.Read(buf)
	return strings.TrimSpace(string(buf[:n]))
}
