package pilots

import (
	"context"
	"net/http"
	"net/url"
)

// Services are machines with a rollout attached.
type Services struct{ c *Client }

func (s *Services) Create(ctx context.Context, req CreateServiceRequest) (*Service, error) {
	var out Service
	return &out, s.c.do(ctx, http.MethodPost, "/v1/services", req, &out)
}

func (s *Services) List(ctx context.Context) ([]Service, error) {
	var out []Service
	return out, s.c.do(ctx, http.MethodGet, "/v1/services", nil, &out)
}

func (s *Services) Get(ctx context.Context, id string) (*Service, error) {
	var out Service
	return &out, s.c.do(ctx, http.MethodGet, "/v1/services/"+url.PathEscape(id), nil, &out)
}

// Deploy cuts a new release over, health-gated, keeping the previous release
// available for rollback.
func (s *Services) Deploy(ctx context.Context, id string, req DeployRequest) (*Release, error) {
	var out Release
	return &out, s.c.do(ctx, http.MethodPost, "/v1/services/"+url.PathEscape(id)+"/deploy", req, &out)
}

func (s *Services) Rollback(ctx context.Context, id string) (*Release, error) {
	var out Release
	return &out, s.c.do(ctx, http.MethodPost, "/v1/services/"+url.PathEscape(id)+"/rollback", nil, &out)
}

// Patch changes a service in place. Env and SecretEnv REPLACE the stored map
// rather than merging into it, and take effect at the next deploy; Replicas is
// reconciled by the autoscaler.
func (s *Services) Patch(ctx context.Context, id string, req UpdateServiceRequest) (*Service, error) {
	var out Service
	return &out, s.c.do(ctx, http.MethodPatch, "/v1/services/"+url.PathEscape(id), req, &out)
}

// Releases lists a service's releases, newest first.
func (s *Services) Releases(ctx context.Context, id string) ([]Release, error) {
	var out []Release
	return out, s.c.do(ctx, http.MethodGet, "/v1/services/"+url.PathEscape(id)+"/releases", nil, &out)
}

// Domains are custom hostnames. Verification is what stops a caller spending
// the fleet's shared certificate rate limit on a name they do not own.
type Domains struct{ c *Client }

// Add answers 201 when the CNAME already points here and 202 when it does not
// yet; either way the response names the target it has to carry.
func (d *Domains) Add(ctx context.Context, req AddDomainRequest) (*DomainResponse, error) {
	var out DomainResponse
	return &out, d.c.do(ctx, http.MethodPost, "/v1/domains", req, &out)
}

func (d *Domains) List(ctx context.Context) ([]DomainResponse, error) {
	var out []DomainResponse
	return out, d.c.do(ctx, http.MethodGet, "/v1/domains", nil, &out)
}

func (d *Domains) Remove(ctx context.Context, hostname string) error {
	return d.c.do(ctx, http.MethodDelete, "/v1/domains/"+url.PathEscape(hostname), nil, nil)
}

// Volumes is persistent, per-write-durable storage.
type Volumes struct{ c *Client }

func (v *Volumes) Create(ctx context.Context, req CreateVolumeRequest) (*Volume, error) {
	var out Volume
	return &out, v.c.do(ctx, http.MethodPost, "/v1/volumes", req, &out)
}

func (v *Volumes) List(ctx context.Context) ([]Volume, error) {
	var out []Volume
	return out, v.c.do(ctx, http.MethodGet, "/v1/volumes", nil, &out)
}

// Compose plans a compose file into ordered steps. Stateless: nothing is
// created, and there is no apps table for it to write to.
type Compose struct{ c *Client }

// Plan returns the ordered steps, or a *ComposePlanError listing every feature
// the planner will not accept.
func (p *Compose) Plan(ctx context.Context, req ComposeRequest) (*ComposePlan, error) {
	var out ComposePlan
	return &out, p.c.do(ctx, http.MethodPost, "/v1/compose/plan", req, &out)
}

// APIKeys mints and revokes API keys. Admin scope.
type APIKeys struct{ c *Client }

// Create returns the plaintext key in Key, on this response and never again.
func (a *APIKeys) Create(ctx context.Context, req CreateAPIKeyRequest) (*APIKeyResponse, error) {
	var out APIKeyResponse
	return &out, a.c.do(ctx, http.MethodPost, "/v1/api-keys", req, &out)
}

func (a *APIKeys) Revoke(ctx context.Context, hash string) (*RevokeResponse, error) {
	var out RevokeResponse
	return &out, a.c.do(ctx, http.MethodPost, "/v1/api-keys/"+url.PathEscape(hash)+"/revoke", nil, &out)
}

// List reports an org's keys. The org is required: keys are per-org and there
// is no fleet-wide listing.
func (a *APIKeys) List(ctx context.Context, org string) ([]APIKeyResponse, error) {
	var out []APIKeyResponse
	return out, a.c.do(ctx, http.MethodGet, query("/v1/api-keys", [2]string{"org", org}), nil, &out)
}

// Quotas are the per-org ceilings enforced on every create. Admin scope.
type Quotas struct{ c *Client }

func (q *Quotas) Get(ctx context.Context, org string) (*QuotaResponse, error) {
	var out QuotaResponse
	return &out, q.c.do(ctx, http.MethodGet, "/v1/quotas/"+url.PathEscape(org), nil, &out)
}

// Put replaces an org's quota. UpdatedAt on the request is ignored; the
// response carries the stored row.
func (q *Quotas) Put(ctx context.Context, org string, quota QuotaResponse) (*QuotaResponse, error) {
	quota.UpdatedAt = 0
	var out QuotaResponse
	return &out, q.c.do(ctx, http.MethodPut, "/v1/quotas/"+url.PathEscape(org), quota, &out)
}

// Usage is the answering host's accrued usage, per org. Admin scope.
type Usage struct{ c *Client }

// Get reports usage over [since, until) in unix seconds. Zero for either means
// the host's default, the last 24 hours.
func (u *Usage) Get(ctx context.Context, since, until int64) (*UsageResponse, error) {
	var out UsageResponse
	path := "/v1/usage"
	pairs := make([][2]string, 0, 2)
	if since > 0 {
		pairs = append(pairs, [2]string{"since", itoa(since)})
	}
	if until > 0 {
		pairs = append(pairs, [2]string{"until", itoa(until)})
	}
	return &out, u.c.do(ctx, http.MethodGet, query(path, pairs...), nil, &out)
}
