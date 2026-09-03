package pilots

import (
	"context"
	"iter"
	"net/http"
	"net/url"
)

// Machines is the one primitive: a sandbox and a production service are the
// same machine with different lifecycle knobs.
type Machines struct{ c *Client }

func (m *Machines) Create(ctx context.Context, req CreateMachineRequest) (*Machine, error) {
	var out Machine
	return &out, m.c.do(ctx, http.MethodPost, "/v1/machines", req, &out)
}

func (m *Machines) List(ctx context.Context) ([]Machine, error) {
	var out []Machine
	return out, m.c.do(ctx, http.MethodGet, "/v1/machines", nil, &out)
}

func (m *Machines) Get(ctx context.Context, id string) (*Machine, error) {
	var out Machine
	return &out, m.c.do(ctx, http.MethodGet, "/v1/machines/"+url.PathEscape(id), nil, &out)
}

func (m *Machines) Destroy(ctx context.Context, id string) error {
	return m.c.do(ctx, http.MethodDelete, "/v1/machines/"+url.PathEscape(id), nil, nil)
}

// Exec runs a command and waits for it. For output that should not be held in
// memory, or a run that lasts minutes, use ExecStream.
func (m *Machines) Exec(ctx context.Context, id string, req ExecRequest) (*ExecResponse, error) {
	var out ExecResponse
	return &out, m.c.do(ctx, http.MethodPost, "/v1/machines/"+url.PathEscape(id)+"/exec", req, &out)
}

// Logs returns the machine's console log.
func (m *Machines) Logs(ctx context.Context, id string) (string, error) {
	return m.c.text(ctx, http.MethodGet, "/v1/machines/"+url.PathEscape(id)+"/logs")
}

// FollowLogs streams the console log line by line. The sequence ends when the
// context is cancelled or the connection drops; a read error is the second
// value of the final pair.
func (m *Machines) FollowLogs(ctx context.Context, id string) (iter.Seq2[string, error], error) {
	req, err := m.c.request(ctx, http.MethodGet,
		query("/v1/machines/"+url.PathEscape(id)+"/logs", [2]string{"follow", "1"}), nil)
	if err != nil {
		return nil, err
	}
	res, err := m.c.send(req)
	if err != nil {
		return nil, err
	}
	return textLines(res), nil
}

// Suspend snapshots the machine and frees its memory. Its URL still resolves,
// and the next request wakes it.
func (m *Machines) Suspend(ctx context.Context, id string) error {
	return m.c.do(ctx, http.MethodPost, "/v1/machines/"+url.PathEscape(id)+"/suspend", nil, nil)
}

func (m *Machines) Wake(ctx context.Context, id string) error {
	return m.c.do(ctx, http.MethodPost, "/v1/machines/"+url.PathEscape(id)+"/wake", nil, nil)
}

// Stop is the non-snapshotting equivalent of Suspend.
func (m *Machines) Stop(ctx context.Context, id string) error {
	return m.c.do(ctx, http.MethodPost, "/v1/machines/"+url.PathEscape(id)+"/stop", nil, nil)
}

func (m *Machines) Start(ctx context.Context, id string) error {
	return m.c.do(ctx, http.MethodPost, "/v1/machines/"+url.PathEscape(id)+"/start", nil, nil)
}

// Checkpoint records a restorable point. ResumeGapMS on the response is how
// long the guest was frozen, which is far less than the call itself takes.
func (m *Machines) Checkpoint(ctx context.Context, id, comment string) (*Checkpoint, error) {
	var out Checkpoint
	return &out, m.c.do(ctx, http.MethodPost, "/v1/machines/"+url.PathEscape(id)+"/checkpoints",
		CheckpointRequest{Comment: comment}, &out)
}

func (m *Machines) ListCheckpoints(ctx context.Context, id string) ([]Checkpoint, error) {
	var out []Checkpoint
	return out, m.c.do(ctx, http.MethodGet, "/v1/machines/"+url.PathEscape(id)+"/checkpoints", nil, &out)
}

// Promote turns a sandbox into a durable service. The machine's URL does not
// change; a custom domain is additive.
func (m *Machines) Promote(ctx context.Context, id string, req PromoteRequest) (*Service, error) {
	var out Service
	return &out, m.c.do(ctx, http.MethodPost, "/v1/machines/"+url.PathEscape(id)+"/promote", req, &out)
}

// Volume reports the volume drive Firecracker actually has, not the one hostd
// meant to set: the difference between the two is a durability guarantee that
// fails silently.
func (m *Machines) Volume(ctx context.Context, id string) (*MachineVolume, error) {
	var out MachineVolume
	return &out, m.c.do(ctx, http.MethodGet, "/v1/machines/"+url.PathEscape(id)+"/volume", nil, &out)
}

// Checkpoints restores and inspects checkpoints.
type Checkpoints struct{ c *Client }

// Restore restores IN PLACE: the same machine, keeping its id, its URL and its
// agent token. A restore that created a machine would mint a new URL.
func (k *Checkpoints) Restore(ctx context.Context, id string) (*Machine, error) {
	var out Machine
	return &out, k.c.do(ctx, http.MethodPost, "/v1/checkpoints/"+url.PathEscape(id)+"/restore", nil, &out)
}

// Get reports a checkpoint's state; Durable flips once the upload lands.
func (k *Checkpoints) Get(ctx context.Context, id string) (*Checkpoint, error) {
	var out Checkpoint
	return &out, k.c.do(ctx, http.MethodGet, "/v1/checkpoints/"+url.PathEscape(id), nil, &out)
}

// Hosts is the fleet as the answering host sees it, from its local replica.
type Hosts struct{ c *Client }

func (h *Hosts) List(ctx context.Context) ([]Host, error) {
	var out []Host
	return out, h.c.do(ctx, http.MethodGet, "/v1/hosts", nil, &out)
}
