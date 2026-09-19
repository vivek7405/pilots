package machines

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/google/uuid"

	"github.com/pilotsrun/pilots/hostd/internal/block"
	"github.com/pilotsrun/pilots/hostd/internal/fc"
	"github.com/pilotsrun/pilots/hostd/internal/netns"
	"github.com/pilotsrun/pilots/hostd/internal/state"
)

// createFromRelease brings a machine up by RESTORING a release's build pair.
//
// This is what makes a deploy fast. A machine created from a built image goes
// through bootMachine -- a cold boot, and the one path in the engine with no
// latency budget on it. A release's first replica pays that once, proves it
// serves, and is checkpointed; every replica after it lands here instead, on
// the same restore path that makes an ordinary create sub-second.
//
// The pair is parented on whatever its headers say it was encoded against --
// the golden template for a release photographed from a template-restored
// replica, nothing at all for one photographed from an image-booted replica
// -- exactly as a checkpoint restore is. The guest in that image carries the
// template's PLACEHOLDER credential, because the rollout resets it before
// snapshotting -- so this installs the machine's own token the same way a
// template restore does.
//
// It deliberately does NOT deliver an environment. The image was captured with
// the application already running and its environment already delivered; a
// second delivery would hand an environment to a process that cannot read it,
// which is the same reason the wake path does not deliver one either.
func (m *Manager) createFromRelease(ctx context.Context, row *state.Machine,
	token, memBuildID, rootfsBuildID, snapKey, imageToken string) (*fc.Machine, error) {

	// Named here rather than discovered inside the restore. A restore needs
	// THREE artifacts -- the memory image, the disk, and the vmstate holding
	// device state and vcpu registers -- and only the first two are build ids.
	// This path used to pass no vmstate key at all, so every restore from a
	// release fetched the empty key and died inside the AWS SDK on "input
	// member Key must not be empty": a message that names neither the release,
	// the machine, nor the artifact that was missing.
	//
	// Refusing here instead. The caller knows which release it is starting and
	// can say so, and a replica that cannot restore has a boot to fall back
	// on, which is slower and correct.
	if snapKey == "" {
		return nil, fmt.Errorf("machines: %s restores memory build %s with no vmstate key; "+
			"the release was photographed before its vmstate was recorded, so it can "+
			"only be booted", row.ID, memBuildID)
	}

	memBuild, err := uuid.Parse(memBuildID)
	if err != nil {
		return nil, fmt.Errorf("machines: release memory build %q: %w", memBuildID, err)
	}

	// What the pair was actually encoded against, read from the builds' own
	// headers rather than assumed to be the golden template.
	//
	// A release photographed from a template-restored replica carries a
	// memory diff and a disk diff on that template. A release photographed
	// from an IMAGE-booted replica does not: bootMachine gives such a machine
	// no memory parent and makes the image build its own disk template, so
	// its checkpoint is a self-contained memory image (header base == build)
	// and its disk is the image itself. Attaching the golden template to
	// either is precisely the mismatch block.SetParent refuses -- both
	// handlers exited before the device came online, and replica 2 of every
	// image deploy failed to come up. So the row is pinned to what the
	// headers name, and the template is then resolved the way a checkpoint
	// restore resolves it, which already knows a nil memory parent.
	memParent, err := m.buildBase(ctx, memBuild)
	if err != nil {
		return nil, fmt.Errorf("machines: release memory build %s: %w", memBuild, err)
	}
	if memParent == memBuild {
		memParent = uuid.Nil
	}
	var rootfsTemplate, rootfsDiff uuid.UUID
	if rootfsBuildID != "" {
		rootfsBuild, err := uuid.Parse(rootfsBuildID)
		if err != nil {
			return nil, fmt.Errorf("machines: release disk build %q: %w", rootfsBuildID, err)
		}
		if rootfsTemplate, err = m.buildBase(ctx, rootfsBuild); err != nil {
			return nil, fmt.Errorf("machines: release disk build %s: %w", rootfsBuild, err)
		}
		if rootfsTemplate != rootfsBuild {
			rootfsDiff = rootfsBuild
		}
	} else {
		t, err := m.EnsureTemplate(ctx)
		if err != nil {
			return nil, err
		}
		rootfsTemplate = t.RootfsBuildID
	}

	// Pin the template this pair is a diff against, for the same reason
	// createFromTemplate does: restoring against a different template returns
	// a guest stitched together from two machines.
	row.TemplateMemBuildID = memParent.String()
	row.TemplateRootfsBuildID = rootfsTemplate.String()
	t, err := m.templateFor(ctx, row)
	if err != nil {
		return nil, err
	}

	backends := fc.Backends{
		MemBuildID:        memBuild,
		MemParentBuildID:  t.MemBuildID,
		RootfsTemplateDir: m.rootfsTemplateDir(t),
		RootfsTemplateID:  t.RootfsBuildID,
		RootfsDiffID:      rootfsDiff,
		CacheRoot:         m.buildDir(),
	}

	fcm, slot, err := m.restoreInstant(ctx, row, backends, snapKey)
	if err != nil {
		return nil, err
	}

	// As the credential the IMAGE carries, which is the placeholder for a
	// release and the parent's own token for a fork. See ImageToken.
	authAs := imageToken
	if authAs == "" {
		authAs = templateToken
	}
	if err := m.installTokenAs(ctx, slot, authAs, token); err != nil {
		m.releaseDiscovery(row.ID)
		_ = fcm.Kill()
		m.pool.Return(slot.Idx)
		return nil, fmt.Errorf("install agent token: %w", err)
	}
	return fcm, nil
}

// buildBase is the build at the root of a build's chain: the build itself when
// it is self-contained (a template, an image, a booted machine's memory
// image), else the template it was diffed against. Every chain here is one
// diff deep, so the base IS the parent to attach.
//
// Read from the header this host already holds when it has one, else fetched
// -- which leaves the header cached for the handler about to open the same
// build. A local header that does not parse is refetched rather than trusted.
func (m *Manager) buildBase(ctx context.Context, id uuid.UUID) (uuid.UUID, error) {
	if f, err := os.Open(filepath.Join(m.buildDir(), id.String(), "header")); err == nil {
		h, err := block.Deserialize(f)
		f.Close()
		if err == nil {
			return h.Metadata.BaseBuildId, nil
		}
	}
	b, err := block.OpenRemoteBuild(ctx, m.opts.BlockStore, id, m.buildDir())
	if err != nil {
		return uuid.Nil, err
	}
	defer b.Close()
	return b.Header().Metadata.BaseBuildId, nil
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
	// Authenticate as the credential the guest currently HAS, and install the
	// placeholder. The arguments read the wrong way round at a glance, which
	// is exactly how they were written the first time: the guest is holding
	// this machine's own token, so that is what opens the door, and the
	// placeholder is what goes in. Reversed, every release snapshot fails with
	// a 401 and the deploy silently falls back to booting every replica.
	return m.installTokenAs(ctx, slot, m.token(machineID), templateToken)
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

// InFlight is how many requests are currently being served by a machine.
//
// Exported for the autoscaler, which is the only caller that needs a number
// rather than a boolean: the idle monitor asks "is anything happening", and
// scaling asks "how close to the limit is this replica".
func (m *Manager) InFlight(machineID string) int { return m.flight.count(machineID) }
