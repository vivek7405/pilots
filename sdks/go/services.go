package pilots

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
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

// Scale changes how big every replica is, and how many there are.
//
// Pass zero for anything to leave it alone. A size change replaces the
// replicas one at a time, at the same release, and drops no request: a replica
// comes up at the new size, passes the same health gate a deploy's does, and
// only then is an old one retired.
//
// A volume-backed service has a held window instead of no window at all,
// because a volume is mounted by one machine at a time and the replacement
// cannot mount it until the old one has let go. Requests arriving then are
// held, the way a request that arrives while a machine is waking is held, so
// they are served late rather than refused.
func (s *Services) Scale(ctx context.Context, id string, replicas, vcpus, memMiB int) (*Service, error) {
	req := UpdateServiceRequest{}
	if replicas > 0 {
		req.Replicas = &replicas
	}
	if vcpus > 0 || memMiB > 0 {
		req.Size = &Size{VCPUs: vcpus, MemMiB: memMiB}
	}
	return s.Patch(ctx, id, req)
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

// ByMachine is Get plus the per-machine breakdown, in Machines. The org totals
// come back too: the breakdown explains an invoice line rather than replacing
// it.
func (u *Usage) ByMachine(ctx context.Context, since, until int64) (*UsageResponse, error) {
	var out UsageResponse
	pairs := make([][2]string, 0, 3)
	if since > 0 {
		pairs = append(pairs, [2]string{"since", itoa(since)})
	}
	if until > 0 {
		pairs = append(pairs, [2]string{"until", itoa(until)})
	}
	pairs = append(pairs, [2]string{"by", "machine"})
	return &out, u.c.do(ctx, http.MethodGet, query("/v1/usage", pairs...), nil, &out)
}

// Snapshot takes a point-in-time copy of a volume.
//
// A clone inside the volume's own filesystem, so no blocks move and it costs
// milliseconds however large the volume is. A running machine is PAUSED for
// the clone: a copy taken while the guest is writing captures a filesystem
// mid-update, which mounts and then fails later.
func (v *Volumes) Snapshot(ctx context.Context, id string) (*SnapshotResponse, error) {
	var out SnapshotResponse
	return &out, v.c.do(ctx, http.MethodPost,
		"/v1/volumes/"+url.PathEscape(id)+"/snapshots", nil, &out)
}

// Snapshots lists a volume's snapshots, newest first.
func (v *Volumes) Snapshots(ctx context.Context, id string) (*SnapshotListResponse, error) {
	var out SnapshotListResponse
	return &out, v.c.do(ctx, http.MethodGet,
		"/v1/volumes/"+url.PathEscape(id)+"/snapshots", nil, &out)
}

// RestoreSnapshot puts a snapshot back as the volume's live image.
//
// Refused while a machine is RUNNING on the volume: replacing the disk under a
// live guest corrupts it, because the guest's cached filesystem metadata
// describes the image that was there a moment ago. A SUSPENDED machine is
// allowed and loses its memory image, so it cold-boots onto the restored disk
// rather than waking with stale filesystem state.
func (v *Volumes) RestoreSnapshot(ctx context.Context, id, snapshot string) (*SnapshotResponse, error) {
	var out SnapshotResponse
	return &out, v.c.do(ctx, http.MethodPost,
		"/v1/volumes/"+url.PathEscape(id)+"/snapshots/"+url.PathEscape(snapshot)+"/restore", nil, &out)
}

// DeleteSnapshot removes one snapshot, freeing the blocks only it still holds.
func (v *Volumes) DeleteSnapshot(ctx context.Context, id, snapshot string) error {
	return v.c.do(ctx, http.MethodDelete,
		"/v1/volumes/"+url.PathEscape(id)+"/snapshots/"+url.PathEscape(snapshot), nil, nil)
}

// HAFragment is the compose text that turns one Postgres into a cluster.
//
// Fetched rather than built here for the reason every recipe is: the generator
// lives beside the planner that has to accept its output, and a second copy in
// a client would drift from it silently.
func (r *Recipes) HAFragment(ctx context.Context, name string, replicas, etcd int) (*ComposeHAFragment, error) {
	var out ComposeHAFragment
	path := query("/v1/recipes/ha/"+url.PathEscape(name),
		[2]string{"replicas", strconv.Itoa(replicas)},
		[2]string{"etcd", strconv.Itoa(etcd)})
	return &out, r.c.do(ctx, http.MethodGet, path, nil, &out)
}

// Grant replaces what every replica of this service may ask its host's broker
// for. See GrantRequest: both fields replace.
func (s *Services) Grant(ctx context.Context, id string, req GrantRequest) (*GrantResponse, error) {
	var out GrantResponse
	return &out, s.c.do(ctx, http.MethodPut,
		"/v1/services/"+url.PathEscape(id)+"/secrets", req, &out)
}

// GrantOf reads what is granted: names and scopes, never values.
func (s *Services) GrantOf(ctx context.Context, id string) (*GrantResponse, error) {
	var out GrantResponse
	return &out, s.c.do(ctx, http.MethodGet,
		"/v1/services/"+url.PathEscape(id)+"/secrets", nil, &out)
}

// RevokeGrant removes it, so the next token this service's replicas ask for is
// refused. Tokens already minted die at their own expiry.
func (s *Services) RevokeGrant(ctx context.Context, id string) error {
	return s.c.do(ctx, http.MethodDelete,
		"/v1/services/"+url.PathEscape(id)+"/secrets", nil, nil)
}

// Env reads a service's environment back, values and all.
//
// The one call that answers with values; everything else returns names. Needs a
// deploy-scoped key, which is the level that already sets them.
func (s *Services) Env(ctx context.Context, id string) (*ServiceEnvResponse, error) {
	var out ServiceEnvResponse
	return &out, s.c.do(ctx, http.MethodGet, "/v1/services/"+url.PathEscape(id)+"/env", nil, &out)
}

// ForkSnapshot makes a NEW volume holding a snapshot's contents, leaving the
// original alone.
//
// What a recovery is built on: restoring in place replaces the data you are
// trying to compare against, and a fork gives you both. It copies bytes, so it
// is proportional to what the volume holds rather than instant.
func (v *Volumes) ForkSnapshot(ctx context.Context, id, snapshot, name string) (*Volume, error) {
	var out Volume
	return &out, v.c.do(ctx, http.MethodPost,
		"/v1/volumes/"+url.PathEscape(id)+"/snapshots/"+url.PathEscape(snapshot)+"/fork",
		map[string]string{"name": name}, &out)
}

// Policy is a volume's snapshot schedule and retention. An empty one means no
// schedule, which is what every volume has until somebody sets one.
func (v *Volumes) Policy(ctx context.Context, id string) (*VolumePolicy, error) {
	var out VolumePolicy
	return &out, v.c.do(ctx, http.MethodGet,
		"/v1/volumes/"+url.PathEscape(id)+"/policy", nil, &out)
}

// SetPolicy schedules snapshots and sets how many are kept.
//
// Nobody takes a manual snapshot before the mistake, which is the whole reason
// this exists: a schedule protects against the failures you did not anticipate,
// and those are the ones that happen.
func (v *Volumes) SetPolicy(ctx context.Context, id string, p VolumePolicy) (*VolumePolicy, error) {
	var out VolumePolicy
	return &out, v.c.do(ctx, http.MethodPut,
		"/v1/volumes/"+url.PathEscape(id)+"/policy", p, &out)
}

// Recipes are the database fragments `pilot add` splices into a compose file.
type Recipes struct{ c *Client }

// Get fetches one. name defaults to the engine; mode is "wal-archive" or
// "durable-volume" and applies to Postgres only; pool defaults on, and applies
// to Postgres only.
//
// The recipe carries no password: SecretNames says what to generate, and the
// caller generates it, so the value never travels.
func (r *Recipes) Get(ctx context.Context, engine, name, mode string, pool bool) (*ComposeRecipe, error) {
	pairs := make([][2]string, 0, 3)
	if name != "" {
		pairs = append(pairs, [2]string{"name", name})
	}
	if mode != "" {
		pairs = append(pairs, [2]string{"mode", mode})
	}
	if !pool {
		pairs = append(pairs, [2]string{"pool", "false"})
	}
	var out ComposeRecipe
	return &out, r.c.do(ctx, http.MethodGet,
		query("/v1/recipes/"+url.PathEscape(engine), pairs...), nil, &out)
}
