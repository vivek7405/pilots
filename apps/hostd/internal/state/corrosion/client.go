// Package corrosion talks to the Corrosion agent that replicates this host's
// view of the cluster.
//
// Corrosion is Fly's gossip-replicated SQLite: every host runs an agent, writes
// go to the local agent, and cr-sqlite merges them across the fleet. That is
// what lets every host serve the full API without a control plane -- a read is
// a local query, not a network call to somewhere that has to be alive.
//
// The merge is last-write-wins PER COLUMN, with no uniqueness constraints and
// no cross-host transactions. Everything in this package is shaped by that:
// see store.go for the single-writer rule it forces, and cache.go for why
// reads come from a subscription rather than a query.
package corrosion

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"time"

	"golang.org/x/net/http2"
)

const (
	// dialTimeout bounds establishing a connection to the local agent. It is
	// on loopback, so this is generous.
	dialTimeout = 5 * time.Second

	// transportRetryBudget bounds the transport's own retries.
	//
	// Deliberately short. The transport retries a request that failed to reach
	// the agent at all; anything longer than a hiccup is the caller's
	// recovery loop to own, because only the caller knows whether retrying is
	// even correct. A long backoff buried in the transport turns a restarting
	// agent into requests that hang for minutes with nothing logged.
	transportRetryBudget = 2 * time.Second
)

// Client is the HTTP API of the local Corrosion agent.
type Client struct {
	baseURL *url.URL
	http    *http.Client
}

// NewClient dials the agent at addr, authenticating with token.
//
// HTTP/2 with prior knowledge -- h2c -- because that is the only thing the
// agent speaks. An ordinary net/http client negotiates HTTP/1.1 and gets
// nowhere, in a way that reads as a connection problem rather than a protocol
// mismatch.
func NewClient(addr, token string) (*Client, error) {
	baseURL, err := url.Parse("http://" + addr)
	if err != nil {
		return nil, fmt.Errorf("corrosion: bad address %q: %w", addr, err)
	}

	transport := &authTransport{
		token: token,
		base: &retryTransport{
			base: &http2.Transport{
				AllowHTTP: true,
				DialTLSContext: func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
					return (&net.Dialer{Timeout: dialTimeout}).DialContext(ctx, network, addr)
				},
			},
			budget: transportRetryBudget,
		},
	}
	return &Client{baseURL: baseURL, http: &http.Client{Transport: transport}}, nil
}

// Statement is one query and its placeholder arguments.
type Statement struct {
	Query  string `json:"query"`
	Params []any  `json:"params"`
}

// ExecResponse is the result of a transaction.
type ExecResponse struct {
	Results []ExecResult `json:"results"`
	Time    float64      `json:"time"`
	Version *uint64      `json:"version"`
}

// ExecResult is one statement's outcome. Error is per statement, and is
// populated even when the request itself succeeded.
type ExecResult struct {
	RowsAffected uint64  `json:"rows_affected"`
	Time         float64 `json:"time"`
	Error        *string `json:"error"`
}

// Exec runs one statement in its own transaction.
func (c *Client) Exec(ctx context.Context, query string, args ...any) (*ExecResult, error) {
	resp, err := c.ExecMulti(ctx, Statement{Query: query, Params: args})
	if err != nil {
		return nil, err
	}
	if len(resp.Results) == 0 {
		return nil, fmt.Errorf("corrosion: no result for %q", query)
	}
	return &resp.Results[0], nil
}

// ExecMulti runs several statements as ONE transaction.
//
// That atomicity is not a convenience. A self-heal claim writes host_id and
// state together, and cr-sqlite merges column by column: split across two
// calls, a row can end up owned by the rescuer while still reporting the state
// the old owner last wrote.
func (c *Client) ExecMulti(ctx context.Context, statements ...Statement) (*ExecResponse, error) {
	body, err := json.Marshal(statements)
	if err != nil {
		return nil, fmt.Errorf("corrosion: marshal statements: %w", err)
	}

	resp, err := c.post(ctx, "/v1/transactions", body)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	// A write can fail in two shapes, and BOTH have to be decoded. The status
	// code alone is not the answer: the agent answers 200 with per-statement
	// errors inside the body, and 500 with a body of the same shape. Trusting
	// the code drops write failures silently, which for a state store means a
	// machine whose row never changed and nothing anywhere saying so.
	switch resp.StatusCode {
	case http.StatusOK:
		var out ExecResponse
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			return nil, fmt.Errorf("corrosion: decode transaction response: %w", err)
		}
		var errs []error
		for i, r := range out.Results {
			if r.Error != nil {
				errs = append(errs, fmt.Errorf("statement %d: %s", i, *r.Error))
			}
		}
		if len(errs) > 0 {
			return &out, fmt.Errorf("corrosion: transaction reported errors: %w", errors.Join(errs...))
		}
		return &out, nil

	case http.StatusInternalServerError:
		raw, readErr := io.ReadAll(resp.Body)
		if readErr != nil {
			return nil, fmt.Errorf("corrosion: read error body: %w", readErr)
		}
		var out ExecResponse
		if json.Unmarshal(raw, &out) == nil &&
			len(out.Results) > 0 && out.Results[0].Error != nil {
			return nil, fmt.Errorf("corrosion: %s", *out.Results[0].Error)
		}
		return nil, fmt.Errorf("corrosion: internal server error: %s", raw)

	default:
		raw, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("corrosion: unexpected status %d: %s", resp.StatusCode, raw)
	}
}

// Query runs a read and returns a cursor over its rows.
//
// The caller must Close the result, and must consult Err after iterating: the
// stream can fail partway, after rows have already been handed over.
func (c *Client) Query(ctx context.Context, query string, args ...any) (*Rows, error) {
	body, err := json.Marshal(Statement{Query: query, Params: args})
	if err != nil {
		return nil, fmt.Errorf("corrosion: marshal query: %w", err)
	}

	resp, err := c.post(ctx, "/v1/queries", body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, fmt.Errorf("corrosion: query returned %d: %s", resp.StatusCode, raw)
	}

	rows, err := newRows(ctx, resp.Body, true)
	if err != nil {
		resp.Body.Close()
		return nil, err
	}
	return rows, nil
}

func (c *Client) post(ctx context.Context, path string, body []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL.JoinPath(path).String(), bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("corrosion: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("corrosion: %s: %w", path, err)
	}
	return resp, nil
}

// authTransport adds the bearer token the agent's [api.authz] section expects.
type authTransport struct {
	base  http.RoundTripper
	token string
}

func (t *authTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if t.token == "" {
		return t.base.RoundTrip(req)
	}
	// A RoundTripper must not modify the request it is given.
	req = req.Clone(req.Context())
	req.Header.Set("Authorization", "Bearer "+t.token)
	return t.base.RoundTrip(req)
}

// retryTransport retries a request that never reached the agent.
//
// Network errors only, and only for a couple of seconds: this covers an agent
// being restarted underneath us, not an agent that is down. Retrying anything
// else would replay writes the agent may already have applied.
type retryTransport struct {
	base   http.RoundTripper
	budget time.Duration
}

func (t *retryTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// A request with a body cannot be replayed unless it can be rewound, which
	// is only true when GetBody is set. net/http sets it for the byte-slice
	// bodies this package uses.
	deadline := time.Now().Add(t.budget)
	wait := 100 * time.Millisecond

	for attempt := 0; ; attempt++ {
		resp, err := t.base.RoundTrip(req)
		if err == nil {
			return resp, nil
		}

		var opErr *net.OpError
		if !errors.As(err, &opErr) || time.Now().After(deadline) || req.GetBody == nil {
			return nil, err
		}
		select {
		case <-req.Context().Done():
			return nil, err
		case <-time.After(wait):
		}
		if wait *= 2; wait > 500*time.Millisecond {
			wait = 500 * time.Millisecond
		}

		body, berr := req.GetBody()
		if berr != nil {
			return nil, err
		}
		req = req.Clone(req.Context())
		req.Body = body
	}
}
