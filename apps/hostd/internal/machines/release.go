package machines

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/vivek7405/pilots/hostd/internal/fc"
	"github.com/vivek7405/pilots/hostd/internal/netns"
	"github.com/vivek7405/pilots/hostd/internal/state"
)

// createFromRelease brings a machine up by RESTORING a release's build pair.
//
// This is what makes a deploy fast. A machine created from a built image goes
// through bootMachine -- a cold boot, and the one path in the engine with no
// latency budget on it. A release's first replica pays that once, proves it
// serves, and is checkpointed; every replica after it lands here instead, on
// the same restore path that makes an ordinary create sub-second.
//
// The pair is a diff against the golden template exactly as a checkpoint's is,
// so it is parented the same way. The guest in that image carries the
// template's PLACEHOLDER credential, because the rollout resets it before
// snapshotting -- so this installs the machine's own token the same way a
// template restore does.
//
// It deliberately does NOT deliver an environment. The image was captured with
// the application already running and its environment already delivered; a
// second delivery would hand an environment to a process that cannot read it,
// which is the same reason the wake path does not deliver one either.
func (m *Manager) createFromRelease(ctx context.Context, row *state.Machine,
	token, memBuildID, rootfsBuildID string) (*fc.Machine, error) {

	t, err := m.EnsureTemplate(ctx)
	if err != nil {
		return nil, err
	}
	memBuild, err := uuid.Parse(memBuildID)
	if err != nil {
		return nil, fmt.Errorf("machines: release memory build %q: %w", memBuildID, err)
	}

	// Pin the template this pair is a diff against, for the same reason
	// createFromTemplate does: restoring against a different template returns
	// a guest stitched together from two machines.
	row.TemplateMemBuildID = t.MemBuildID.String()
	row.TemplateRootfsBuildID = t.RootfsBuildID.String()

	backends := fc.Backends{
		MemBuildID:        memBuild,
		MemParentBuildID:  t.MemBuildID,
		RootfsTemplateDir: m.rootfsTemplateDir(t),
		CacheRoot:         m.buildDir(),
	}
	if rootfsBuildID != "" {
		if backends.RootfsDiffID, err = uuid.Parse(rootfsBuildID); err != nil {
			return nil, fmt.Errorf("machines: release disk build %q: %w", rootfsBuildID, err)
		}
	}

	fcm, slot, err := m.restoreInstant(ctx, row, backends, "")
	if err != nil {
		return nil, err
	}

	if err := m.installToken(ctx, slot, token); err != nil {
		m.releaseDiscovery(row.ID)
		_ = fcm.Kill()
		m.pool.Return(slot.Idx)
		return nil, fmt.Errorf("install agent token: %w", err)
	}
	return fcm, nil
}

// ResetAgentToken puts a guest's credential back to the placeholder the golden
// template ships.
//
// Called before a release snapshot. Every machine restored from that image
// installs its own token by authenticating as the placeholder, so an image
// carrying THIS machine's token would lock every later replica out of its own
// agent -- and it would fail at install time, long after the deploy reported
// the snapshot succeeded.
func (m *Manager) ResetAgentToken(ctx context.Context, machineID string) error {
	slot, ok := m.SlotFor(machineID)
	if !ok {
		return fmt.Errorf("machines: %s holds no slot on this host", machineID)
	}
	return m.installTokenAs(ctx, slot, templateToken, m.token(machineID))
}

// AppAddr is where this host reaches a machine's application port.
//
// Empty when the machine holds no slot here, which is the honest answer for a
// suspended machine or one owned by another host -- a health check has to
// distinguish "not serving" from "not mine to probe".
func (m *Manager) AppAddr(machineID string) (string, bool) {
	slot, ok := m.SlotFor(machineID)
	if !ok {
		return "", false
	}
	return fmt.Sprintf("%s:%d", slot.HostIP, netns.GuestAppPort), true
}
