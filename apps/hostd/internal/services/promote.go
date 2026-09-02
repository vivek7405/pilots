package services

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/vivek7405/pilots/hostd/internal/api"
	"github.com/vivek7405/pilots/hostd/internal/state"
)

// Promote turns a running sandbox into a durable service without touching the
// machine.
//
// The machine row is deliberately NOT rewritten: same id, same
// <name>.pilotrun.app, same agent token. That is the "identity preserved" line
// in the parity table and the reason the sandbox's URL keeps working -- an
// agent that has been iterating against a sandbox for an hour keeps the URL it
// has been using, and every checkpoint it took still restores into the same
// machine. A promote that mints a new machine has failed even if everything
// serves.
//
// What it does instead: checkpoint the sandbox (which is what a release IS --
// a memory and disk build pair), write a release naming that pair, and point a
// service row at it. The sandbox becomes replica one of its own service.
func (m *Manager) Promote(ctx context.Context, machineID string, req api.PromoteRequest) (*state.Service, error) {
	row, err := m.opts.Store.GetMachine(ctx, machineID)
	if err != nil {
		return nil, err
	}
	if row.ServiceID != "" {
		if svc, err := m.opts.Store.GetService(ctx, row.ServiceID); err == nil && svc.ReleaseID != "" {
			return nil, fmt.Errorf("services: machine %s is already part of service %s",
				machineID, svc.ID)
		}
	}

	replicas := req.Replicas
	if replicas < 1 {
		replicas = 1
	}

	// The sandbox's own service row, minted by its create, is what this
	// promotes -- reusing it keeps the machine's environment, sealed secrets
	// included, exactly where it already is. A sandbox created before service
	// rows existed gets a fresh one.
	svc := &state.Service{}
	if row.ServiceID != "" {
		if existing, err := m.opts.Store.GetService(ctx, row.ServiceID); err == nil {
			svc = existing
		}
	}
	if svc.ID == "" {
		hosts, _ := m.opts.Store.ListHosts(ctx)
		svc.ID = state.NewOwnedID("svc_", m.opts.HostID, state.LiveHosts(hosts))
		svc.CreatedAt = time.Now().Unix()
	}
	svc.Name = row.Name
	svc.App = row.App
	svc.Replicas = replicas
	// The sandbox's URL is already <name>.<domain>; the service adopts that
	// label rather than minting a second one.
	svc.Domain = row.Name
	if req.CustomDomain != "" {
		svc.CustomDomain = req.CustomDomain
	}
	if req.Health != nil {
		raw, _ := json.Marshal(req.Health)
		svc.Health = string(raw)
	}

	// Checkpoint FIRST, so the release describes a machine that was serving.
	// Reset the credential before it for the same reason a rollout does: every
	// replica restored from this image installs its own token by
	// authenticating as the placeholder.
	rel := &state.Release{
		ID:        "rel_" + uuid.NewString(),
		ServiceID: svc.ID,
		CreatedAt: time.Now().Unix(),
		Healthy:   true, // it is serving right now; that is the proof
	}
	if err := m.snapshotRelease(ctx, machineID, rel); err != nil {
		return nil, fmt.Errorf("services: promoting %s: %w", machineID, err)
	}
	svc.ReleaseID = rel.ID

	if err := m.opts.Store.PutService(ctx, svc); err != nil {
		return nil, err
	}
	if err := m.opts.Store.PutRelease(ctx, rel); err != nil {
		return nil, err
	}

	// Bind the machine to the release it came from, so scale-up and rollback
	// can find it. This writes the machine's own row, which its owning host is
	// entitled to do -- and the fields it touches are bookkeeping, not
	// identity: the id, name, domain and token are untouched.
	row.ServiceID = svc.ID
	row.ReleaseID = rel.ID
	row.UpdatedAt = time.Now().Unix()
	if err := m.opts.Store.PutMachine(ctx, row); err != nil {
		return nil, err
	}

	// Any additional replicas restore from the release, exactly as a deploy's
	// do. The promoted machine itself is already replica one and is not
	// touched.
	for i := 1; i < replicas; i++ {
		if _, err := m.createReplica(ctx, svc, rel, rel.MemBuildID != ""); err != nil {
			return nil, fmt.Errorf("services: replica %d of promoted %s: %w", i+1, machineID, err)
		}
	}
	return svc, nil
}
