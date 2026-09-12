// Package pilots is the Go client for the pilots API: instant sandboxes and
// durable production services on one primitive, Firecracker microVMs.
//
//	c := pilots.New(os.Getenv("PILOT_API_KEY"))
//
//	m, err := c.Machines.Create(ctx, pilots.CreateMachineRequest{Name: "demo"})
//	if err != nil {
//		return err
//	}
//
//	s, err := c.Machines.ExecStream(ctx, m.ID,
//		[]string{"bash", "-c", "echo hi"}, pilots.ExecStreamOptions{})
//	if err != nil {
//		return err
//	}
//	stdout, stderr, code, err := s.Output()
//
// Every host serves this identical API, so the base URL is any host in the
// fleet: there is no control-plane tier to be down, and a write that arrives
// at the wrong host is forwarded by hostd itself.
//
// Nothing here retries, pools or rate-limits. A caller who wants any of those
// wraps the *http.Client and passes it with WithHTTPClient.
package pilots

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
)

// DefaultBaseURL is used when neither WithBaseURL nor PILOT_API_URL says
// otherwise.
const DefaultBaseURL = "https://api.pilotrun.app"

// Client is the entry point. Construct it with New and reuse it; it is safe
// for concurrent use.
type Client struct {
	apiKey     string
	baseURL    string
	httpClient *http.Client
	// org narrows every request to one org. See WithOrg.
	org string

	Machines    *Machines
	Builders    *Builders
	Checkpoints *Checkpoints
	Builds      *Builds
	Services    *Services
	Domains     *Domains
	Volumes     *Volumes
	Hosts       *Hosts
	APIKeys     *APIKeys
	Quotas      *Quotas
	Usage       *Usage
	Compose     *Compose
	Recipes     *Recipes
}

// Option customises a Client.
type Option func(*Client)

// WithBaseURL overrides the API base URL.
func WithBaseURL(u string) Option {
	return func(c *Client) { c.baseURL = strings.TrimRight(u, "/") }
}

// WithOrg makes an ADMIN key act as one org: every request carries ?org=,
// which hostd reads as the org to create rows in, charge quota to, and narrow
// every read by.
//
// For a process that serves many orgs from one operator key -- the dashboard
// is the case this exists for -- so that the rows it creates belong to the
// person who asked for them rather than to the ops org. hostd ignores the
// parameter on a tenant-scoped key, which has exactly one org already, so
// setting this on one changes nothing.
func WithOrg(org string) Option {
	return func(c *Client) { c.org = org }
}

// WithHTTPClient overrides the underlying *http.Client. The websocket dial
// does not use it: a stream has no deadline to inherit.
func WithHTTPClient(hc *http.Client) Option {
	return func(c *Client) { c.httpClient = hc }
}

// New returns a Client authenticated with the given API key.
func New(apiKey string, opts ...Option) *Client {
	base := os.Getenv("PILOT_API_URL")
	if base == "" {
		base = DefaultBaseURL
	}
	c := &Client{
		apiKey:  apiKey,
		baseURL: strings.TrimRight(base, "/"),
		// No client-level deadline on purpose. A build streams for minutes, an
		// exec carries its own timeout_ms, and a checkpoint pauses a guest --
		// any fixed ceiling here would truncate one of them into a network
		// error rather than a result. Deadlines belong on the caller's
		// context, which is where a Go caller looks for them.
		httpClient: &http.Client{},
	}
	for _, opt := range opts {
		opt(c)
	}
	c.Machines = &Machines{c: c}
	c.Builders = &Builders{c: c}
	c.Checkpoints = &Checkpoints{c: c}
	c.Builds = &Builds{c: c}
	c.Services = &Services{c: c}
	c.Domains = &Domains{c: c}
	c.Volumes = &Volumes{c: c}
	c.Hosts = &Hosts{c: c}
	c.APIKeys = &APIKeys{c: c}
	c.Quotas = &Quotas{c: c}
	c.Usage = &Usage{c: c}
	c.Compose = &Compose{c: c}
	c.Recipes = &Recipes{c: c}
	return c
}

// BaseURL is the fleet endpoint this client talks to.
func (c *Client) BaseURL() string { return c.baseURL }

// APIKey is the key this client authenticates with.
func (c *Client) APIKey() string { return c.apiKey }

// Health is the one route that needs no key.
func (c *Client) Health(ctx context.Context) (*HealthResponse, error) {
	var out HealthResponse
	return &out, c.do(ctx, http.MethodGet, "/v1/health", nil, &out)
}

// Whoami is the org, scopes and host this client's key resolves to. The one
// route a key can call to learn about itself.
func (c *Client) Whoami(ctx context.Context) (*WhoamiResponse, error) {
	var out WhoamiResponse
	return &out, c.do(ctx, http.MethodGet, "/v1/whoami", nil, &out)
}

// Plan asks the host what a directory is, from a tar of it.
//
// A method on Client rather than under Compose or Services, because it is the
// front door: it is what a caller reaches for before it knows whether the
// directory is a compose project, a Dockerfile or a framework the platform
// recognises. app names the app; empty falls back to package.json's name and
// then to "app".
func (c *Client) Plan(ctx context.Context, contextTar io.Reader, app string) (*ComposePlanResponse, error) {
	path := "/v1/plan"
	if app != "" {
		path = query(path, [2]string{"app", app})
	}
	req, err := c.request(ctx, http.MethodPost, path, contextTar)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-tar")
	req.Header.Set("Accept", "application/json")

	res, err := c.send(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	var out ComposePlanResponse
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("pilots: decoding the plan: %w", err)
	}
	return &out, nil
}

// PlanRepo asks the host what a REPOSITORY is, naming it rather than sending
// it. The host fetches the ref through the fleet's GitHub App, the same path a
// push takes.
//
// For a caller that holds no repository bytes. A fleet with no App configured
// answers not_configured and says to send a tar instead.
func (c *Client) PlanRepo(ctx context.Context, ref RepoRef, app string) (*ComposePlanResponse, error) {
	path := "/v1/plan"
	if app != "" {
		path = query(path, [2]string{"app", app})
	}
	body, err := json.Marshal(ref)
	if err != nil {
		return nil, fmt.Errorf("pilots: encoding the repository: %w", err)
	}
	var out ComposePlanResponse
	return &out, c.do(ctx, http.MethodPost, path, json.RawMessage(body), &out)
}

// ConnectRepo ties a repository to an org, which is what lets that org's own
// key name it in a {repo, ref} build or plan.
//
// ADMIN-SCOPED: the proof that an org controls a repository is held at GitHub,
// so the connection is asserted once by a party that can prove it and hostd
// records it. The org is the one this client acts as -- from the key, or from
// the org this client was built with -- never a field in the body.
//
// Idempotent: the row is write-once, so connecting twice answers the
// connection that is already there rather than moving it.
//
// It exists because the 403 a caller gets for naming an unconnected repository
// says to POST /v1/repos, and a Go caller told to do that needs something to
// call. See Client.ListRepos for the read.
func (c *Client) ConnectRepo(ctx context.Context, repo string) (*RepoLinkResponse, error) {
	body, err := json.Marshal(ConnectRepoRequest{Repo: repo})
	if err != nil {
		return nil, fmt.Errorf("pilots: encoding the repository: %w", err)
	}
	var out RepoLinkResponse
	return &out, c.do(ctx, http.MethodPost, "/v1/repos", json.RawMessage(body), &out)
}

// ListRepos returns the repositories this client's org may name.
//
// Readable with a deploy-scoped key, unlike ConnectRepo: a caller refused a
// build has to be able to see what it IS connected to. Never nil on success.
func (c *Client) ListRepos(ctx context.Context) ([]RepoLinkResponse, error) {
	var out RepoLinkListResponse
	if err := c.do(ctx, http.MethodGet, "/v1/repos", nil, &out); err != nil {
		return nil, err
	}
	if out.Repos == nil {
		return []RepoLinkResponse{}, nil
	}
	return out.Repos, nil
}

// request builds an authenticated request. body may be nil.
//
// The org narrowing is applied HERE rather than at each call site, because it
// has to reach every route: a client acting as an org must create as it, be
// charged as it, and read as it, and a route that forgot the parameter would
// create a row its own reads cannot see.
func (c *Client) request(ctx context.Context, method, path string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+withOrg(path, c.org), body)
	if err != nil {
		return nil, fmt.Errorf("pilots: building the request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	return req, nil
}

// withOrg appends ?org= to a path that may already carry a query.
func withOrg(path, org string) string {
	if org == "" {
		return path
	}
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	return path + sep + "org=" + url.QueryEscape(org)
}

// send performs a request and maps any non-2xx onto the error model. The
// caller closes the body.
func (c *Client) send(req *http.Request) (*http.Response, error) {
	res, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("pilots: %s %s: %w", req.Method, req.URL.Path, err)
	}
	if res.StatusCode >= 200 && res.StatusCode < 300 {
		return res, nil
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	return nil, toError(res.StatusCode, body)
}

// do performs a JSON call. in is marshalled when non-nil; out is decoded when
// non-nil and the response has a body.
func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		buf, err := json.Marshal(in)
		if err != nil {
			return fmt.Errorf("pilots: marshalling the request: %w", err)
		}
		body = bytes.NewReader(buf)
	}
	req, err := c.request(ctx, method, path, body)
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if out != nil {
		req.Header.Set("Accept", "application/json")
	}

	res, err := c.send(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if out == nil || res.StatusCode == http.StatusNoContent {
		_, _ = io.Copy(io.Discard, res.Body)
		return nil
	}
	if err := json.NewDecoder(res.Body).Decode(out); err != nil {
		return fmt.Errorf("pilots: decoding %s %s: %w", method, path, err)
	}
	return nil
}

// text performs a call whose response is read whole as text.
func (c *Client) text(ctx context.Context, method, path string) (string, error) {
	req, err := c.request(ctx, method, path, nil)
	if err != nil {
		return "", err
	}
	res, err := c.send(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		return "", fmt.Errorf("pilots: reading %s %s: %w", method, path, err)
	}
	return string(body), nil
}

// toError maps a failed response onto the narrowest error type that fits it.
func toError(status int, body []byte) error {
	base := &Error{StatusCode: status, Body: string(body)}
	var envelope struct {
		Error       string               `json:"error"`
		Code        string               `json:"code"`
		Next        string               `json:"next"`
		Details     json.RawMessage      `json:"details"`
		Quota       string               `json:"quota"`
		Limit       int64                `json:"limit"`
		Used        int64                `json:"used"`
		Scope       string               `json:"scope"`
		Unsupported []ComposeUnsupported `json:"unsupported"`
	}
	if err := json.Unmarshal(body, &envelope); err == nil {
		base.Message = envelope.Error
		base.Code = envelope.Code
		base.Next = envelope.Next
		base.Details = envelope.Details
	}

	switch {
	case status == http.StatusTooManyRequests && envelope.Quota != "":
		return &QuotaExceeded{
			Quota: envelope.Quota, Limit: envelope.Limit, Used: envelope.Used,
			Scope: envelope.Scope, Err: base,
		}
	case status == http.StatusBadRequest && len(envelope.Unsupported) > 0:
		return &ComposePlanError{
			Message: envelope.Error, Code: envelope.Code, Next: envelope.Next,
			Unsupported: envelope.Unsupported,
		}
	case envelope.Code == "health_gate_failed":
		var d HealthGateDetails
		_ = json.Unmarshal(envelope.Details, &d)
		return &HealthGateFailed{Details: d, Err: base}
	case envelope.Code == "unknown_framework":
		var d ComposeUnknownDetails
		_ = json.Unmarshal(envelope.Details, &d)
		return &UnknownFramework{Details: d, Err: base}
	default:
		return base
	}
}

// query renders parameters onto a path, skipping empty values.
func query(path string, pairs ...[2]string) string {
	q := url.Values{}
	for _, p := range pairs {
		if p[1] != "" {
			q.Set(p[0], p[1])
		}
	}
	if len(q) == 0 {
		return path
	}
	return path + "?" + q.Encode()
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
