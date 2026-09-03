// Package pilots is the Go client for the pilots API: instant sandboxes and
// durable production services on one primitive, Firecracker microVMs.
//
//	c := pilots.New(os.Getenv("PILOT_API_KEY"))
//	m, err := c.Machines.Create(ctx, pilots.CreateMachineRequest{Name: "demo"})
//	out, _, code, err := mustStream(c.Machines.ExecStream(ctx, m.ID,
//		[]string{"bash", "-c", "echo hi"}, pilots.ExecStreamOptions{}))
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
	"time"
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

	Machines    *Machines
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
}

// Option customises a Client.
type Option func(*Client)

// WithBaseURL overrides the API base URL.
func WithBaseURL(u string) Option {
	return func(c *Client) { c.baseURL = strings.TrimRight(u, "/") }
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
		// Generous rather than absent: a create restores a snapshot and a
		// checkpoint pauses a guest, neither of which is a millisecond call.
		httpClient: &http.Client{Timeout: 60 * time.Second},
	}
	for _, opt := range opts {
		opt(c)
	}
	c.Machines = &Machines{c: c}
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

// request builds an authenticated request. body may be nil.
func (c *Client) request(ctx context.Context, method, path string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		return nil, fmt.Errorf("pilots: building the request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	return req, nil
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
		Quota       string               `json:"quota"`
		Limit       int64                `json:"limit"`
		Used        int64                `json:"used"`
		Scope       string               `json:"scope"`
		Unsupported []ComposeUnsupported `json:"unsupported"`
	}
	if err := json.Unmarshal(body, &envelope); err == nil {
		base.Message = envelope.Error
	}

	switch {
	case status == http.StatusTooManyRequests && envelope.Quota != "":
		return &QuotaExceeded{
			Quota: envelope.Quota, Limit: envelope.Limit, Used: envelope.Used,
			Scope: envelope.Scope, Err: base,
		}
	case status == http.StatusBadRequest && len(envelope.Unsupported) > 0:
		return &ComposePlanError{Message: envelope.Error, Unsupported: envelope.Unsupported}
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
