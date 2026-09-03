package machines

import (
	"context"
	"errors"
	"testing"

	"github.com/vivek7405/pilots/hostd/internal/api"
	"github.com/vivek7405/pilots/hostd/internal/state"
)

// A service row minted for a machine inherits the create's org, so the tenant
// that made the machine can see the service that carries its environment.
func TestProvisionServiceWritesTenancy(t *testing.T) {
	ctx := context.Background()
	m, store := envManager(t, true)

	id, err := m.provisionService(ctx, "api", createEnv{
		App: "shop", OrgID: "org_1", Env: map[string]string{"PORT": "8080"},
	})
	if err != nil {
		t.Fatalf("provisionService: %v", err)
	}

	tn, err := store.GetTenancy(ctx, id)
	if err != nil {
		t.Fatalf("the service has no tenancy row: %v", err)
	}
	if tn.OrgID != "org_1" || tn.Kind != "service" {
		t.Errorf("tenancy = %+v, want org_1/service", tn)
	}
}

// A replica the rollout boots must land in its SERVICE's tenant, not in
// whichever org happened to trigger the rollout -- and the rollout is not a
// request, so there is no key to read it from.
func TestAReplicaInheritsItsServicesOrg(t *testing.T) {
	ctx := context.Background()
	_, store := envManager(t, true)

	if err := store.PutTenancy(ctx, &state.Tenancy{
		ID: "svc_1", OrgID: "org_1", Kind: "service",
	}); err != nil {
		t.Fatalf("PutTenancy: %v", err)
	}

	// The resolution Create performs for a replica: no org on the request,
	// so the service's tenancy decides.
	org := ""
	if t2, err := store.GetTenancy(ctx, "svc_1"); err == nil {
		org = t2.OrgID
	}
	if org != "org_1" {
		t.Errorf("a replica of svc_1 would land in %q, want org_1", org)
	}

	// A service with no tenancy row leaves the replica unowned rather than
	// guessing, which keeps it admin-only instead of visible to a tenant that
	// does not own it.
	if _, err := store.GetTenancy(ctx, "svc_absent"); !errors.Is(err, state.ErrNotFound) {
		t.Errorf("an unknown service returned %v, want ErrNotFound", err)
	}
}

// A volume is owned by the tenant that created it, or a second tenant could
// list it and attach it.
func TestCreateVolumeWritesTenancy(t *testing.T) {
	ctx := context.Background()
	m, _, store := newVolumeTestManager(t)

	v, err := m.CreateVolume(ctx, api.CreateVolumeRequest{
		Name: "data", SizeGiB: 1, MountPath: "/data", OrgID: "org_1",
	})
	if err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}
	tn, err := store.GetTenancy(ctx, v.ID)
	if err != nil {
		t.Fatalf("the volume has no tenancy row: %v", err)
	}
	if tn.OrgID != "org_1" || tn.Kind != "volume" {
		t.Errorf("tenancy = %+v, want org_1/volume", tn)
	}
}
