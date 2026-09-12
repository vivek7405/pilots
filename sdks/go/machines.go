package pilots

import (
	"context"
	"iter"
	"net/http"
	"net/url"
	"strconv"
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

// Update changes who may reach the machine's URL.
func (m *Machines) Update(ctx context.Context, id string, req UpdateMachineRequest) (*Machine, error) {
	var out Machine
	if err := m.c.do(ctx, http.MethodPatch, "/v1/machines/"+url.PathEscape(id), req, &out); err != nil {
		return nil, err
	}
	return &out, nil
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

// Stop is Suspend under the name every other platform's CLI uses.
func (m *Machines) Stop(ctx context.Context, id string) error {
	return m.c.do(ctx, http.MethodPost, "/v1/machines/"+url.PathEscape(id)+"/stop", nil, nil)
}

// Start is Wake under the name every other platform's CLI uses.
func (m *Machines) Start(ctx context.Context, id string) error {
	return m.c.do(ctx, http.MethodPost, "/v1/machines/"+url.PathEscape(id)+"/start", nil, nil)
}

// Resize boots a machine again at a new size, in place: same id, same URL,
// same disk, same volume.
//
// A boot rather than a resume, because a memory image cannot be loaded into a
// differently-sized VM, so the machine loses what was in memory. Pass zero for
// a dimension to leave it alone.
func (m *Machines) Resize(ctx context.Context, id string, vcpus, memMiB int) (*Machine, error) {
	var out Machine
	return &out, m.c.do(ctx, http.MethodPost, "/v1/machines/"+url.PathEscape(id)+"/resize",
		ResizeMachineRequest{VCPUs: vcpus, MemMiB: memMiB}, &out)
}

// Fork makes new machines from this one's exact state.
//
// Each fork comes up with the source's processes already running and its memory
// already warm: an agent that spent two minutes installing dependencies and
// loading a model forks into ten machines that all begin from that moment.
//
// A RUNNING source is checkpointed first, in place, keeping its id and URL. A
// SUSPENDED source is forked without being woken at all.
func (m *Machines) Fork(ctx context.Context, id string, req ForkRequest) (*ForkResponse, error) {
	var out ForkResponse
	return &out, m.c.do(ctx, http.MethodPost,
		"/v1/machines/"+url.PathEscape(id)+"/fork", req, &out)
}

// Processes lists what a machine is running.
//
// A machine runs a NAMED SET of processes: an image's own command is the
// process `app`, and a compose file or a runtime registration can add more.
// The point of the names is that one can be restarted without touching the
// others.
func (m *Machines) Processes(ctx context.Context, id string) ([]Process, error) {
	var out struct {
		Processes []Process `json:"processes"`
	}
	err := m.c.do(ctx, http.MethodGet, "/v1/machines/"+url.PathEscape(id)+"/processes", nil, &out)
	return out.Processes, err
}

// StartProcess, StopProcess and RestartProcess act on ONE process. Restarting
// the dev server in a machine must not take down the database beside it.
func (m *Machines) StartProcess(ctx context.Context, id, name string) error {
	return m.processAction(ctx, id, name, "start")
}

func (m *Machines) StopProcess(ctx context.Context, id, name string) error {
	return m.processAction(ctx, id, name, "stop")
}

func (m *Machines) RestartProcess(ctx context.Context, id, name string) error {
	return m.processAction(ctx, id, name, "restart")
}

func (m *Machines) processAction(ctx context.Context, id, name, action string) error {
	return m.c.do(ctx, http.MethodPost,
		"/v1/machines/"+url.PathEscape(id)+"/processes/"+url.PathEscape(name)+"/"+action, nil, nil)
}

// ProcessLogs is one process's captured output, most recent last. tail is a
// line count; zero means everything the guest still holds.
func (m *Machines) ProcessLogs(ctx context.Context, id, name string, tail int) (string, error) {
	path := "/v1/machines/" + url.PathEscape(id) + "/processes/" + url.PathEscape(name) + "/logs"
	if tail > 0 {
		path = query(path, [2]string{"tail", strconv.Itoa(tail)})
	}
	return m.c.text(ctx, http.MethodGet, path)
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

// Fork makes new machines from a checkpoint, rather than from a machine's
// current state. The same restore, addressed by the moment rather than by the
// machine.
func (k *Checkpoints) Fork(ctx context.Context, id string, req ForkRequest) (*ForkResponse, error) {
	var out ForkResponse
	return &out, k.c.do(ctx, http.MethodPost,
		"/v1/checkpoints/"+url.PathEscape(id)+"/fork", req, &out)
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

// Drain moves every machine off a host, so it can be rebooted or retired
// without taking its machines down with it.
//
// Each machine is suspended on that host and restored on another, keeping its
// id, its name and its URL. A request arriving mid-move is held and served
// late, never refused. The host goes on refusing new machines afterwards,
// which is the point; Undrain lets it take work again.
//
// Admin-scoped: a drain moves every org's machines at once.
func (h *Hosts) Drain(ctx context.Context, hostID string) (*DrainReport, error) {
	var out DrainReport
	return &out, h.c.do(ctx, http.MethodPost,
		"/v1/hosts/"+url.PathEscape(hostID)+"/drain", nil, &out)
}

// DrainStatus reports whether a host is draining and what is still on it.
func (h *Hosts) DrainStatus(ctx context.Context, hostID string) (*DrainReport, error) {
	var out DrainReport
	return &out, h.c.do(ctx, http.MethodGet,
		"/v1/hosts/"+url.PathEscape(hostID)+"/drain", nil, &out)
}

// Undrain lets a drained host take machines again. Nothing moves back.
func (h *Hosts) Undrain(ctx context.Context, hostID string) error {
	return h.c.do(ctx, http.MethodDelete,
		"/v1/hosts/"+url.PathEscape(hostID)+"/drain", nil, nil)
}

// Egress is every address this org's outbound traffic can leave from.
//
// What to hand anything that allowlists by source address. One per host that
// manages egress, because the address is derived from the host's own prefix;
// empty on a fleet where no host has been given one, in which case traffic
// leaves from each host's shared address.
//
// The set changes only when a host joins or leaves the fleet, never when this
// org's machines are created, destroyed, resized, rolled or moved.
func (h *Hosts) Egress(ctx context.Context) (*EgressResponse, error) {
	var out EgressResponse
	return &out, h.c.do(ctx, http.MethodGet, "/v1/egress", nil, &out)
}
