package machines

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"testing"

	"github.com/vivek7405/pilots/hostd/internal/seal"
	"github.com/vivek7405/pilots/hostd/internal/state"
)

func envManager(t *testing.T, withKey bool) (*Manager, state.Store) {
	t.Helper()

	store, err := state.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { store.Close() })

	var key seal.Key
	if withKey {
		raw := make([]byte, seal.KeyBytes)
		for i := range raw {
			raw[i] = byte(i)
		}
		if key, err = seal.ParseKey(base64.StdEncoding.EncodeToString(raw)); err != nil {
			t.Fatal(err)
		}
	}
	return &Manager{opts: Options{HostID: "host-a", Store: store, FleetKey: key}}, store
}

// Corrosion replicates every row to every host, so a secret written in the
// clear to one is a secret on all of them and in every backup. The row must
// carry the ciphertext and nothing else.
func TestSecretsAreSealedBeforeTheRowIsWritten(t *testing.T) {
	ctx := context.Background()
	m, store := envManager(t, true)

	id, err := m.provisionService(ctx, "api", createEnv{
		App:       "shop",
		Env:       map[string]string{"PORT": "8080"},
		SecretEnv: map[string]string{"DATABASE_URL": "postgres://user:hunter2@db.internal/app"},
	})
	if err != nil {
		t.Fatalf("provisionService: %v", err)
	}

	svc, err := store.GetService(ctx, id)
	if err != nil {
		t.Fatalf("GetService: %v", err)
	}
	whole := svc.ID + svc.Name + svc.App + svc.Env + svc.EnvSealed
	for _, leak := range []string{"hunter2", "postgres://", "DATABASE_URL"} {
		if strings.Contains(whole, leak) {
			t.Errorf("%q is legible in the stored row: env=%q sealed=%q",
				leak, svc.Env, svc.EnvSealed)
		}
	}
	// The non-secret half is deliberately readable: it is not a secret, and
	// hiding it would mean a fleet key was needed to read a port number.
	if !strings.Contains(svc.Env, "8080") {
		t.Errorf("the non-secret env did not survive: %q", svc.Env)
	}
}

// What the application receives is read back out of the row rather than
// carried through from the request, so a seal that did not round trip fails
// the create rather than producing a machine that cannot be redeployed.
func TestResolveEnvMergesBothHalves(t *testing.T) {
	ctx := context.Background()
	m, _ := envManager(t, true)

	id, err := m.provisionService(ctx, "api", createEnv{
		App: "shop",
		Env: map[string]string{"PORT": "8080", "DATABASE_URL": "unset"},
		// The same name in both halves is exactly what a compose file has
		// before the real value exists, so the secret has to win.
		SecretEnv: map[string]string{"DATABASE_URL": "postgres://real"},
	})
	if err != nil {
		t.Fatal(err)
	}

	env, err := m.resolveEnv(ctx, id)
	if err != nil {
		t.Fatalf("resolveEnv: %v", err)
	}
	if env["PORT"] != "8080" {
		t.Errorf("PORT is %q", env["PORT"])
	}
	if env["DATABASE_URL"] != "postgres://real" {
		t.Errorf("the sealed value did not win the collision: %q", env["DATABASE_URL"])
	}
}

// A host with no fleet key must refuse the create. Storing the plaintext
// instead would replicate it fleet-wide with nothing anywhere reporting it.
func TestAHostWithNoFleetKeyRefusesSecrets(t *testing.T) {
	ctx := context.Background()
	m, _ := envManager(t, false)

	if _, err := m.provisionService(ctx, "api", createEnv{
		SecretEnv: map[string]string{"TOKEN": "shh"},
	}); err == nil {
		t.Fatal("a host with no fleet key accepted a secret")
	} else if !strings.Contains(err.Error(), "fleet key") {
		t.Errorf("the error does not name the cause: %v", err)
	}

	// Non-secret values are unaffected: a key is only needed for secrets.
	if _, err := m.provisionService(ctx, "api", createEnv{
		App: "shop", Env: map[string]string{"PORT": "8080"},
	}); err != nil {
		t.Errorf("a plain environment was refused without a key: %v", err)
	}
}

// A bare sandbox is not a service. An empty row would replicate to every host
// in the fleet to say nothing.
func TestAMachineWithNoEnvironmentGetsNoServiceRow(t *testing.T) {
	ctx := context.Background()
	m, _ := envManager(t, true)

	id, err := m.provisionService(ctx, "scratch", createEnv{})
	if err != nil {
		t.Fatal(err)
	}
	if id != "" {
		t.Errorf("a bare sandbox minted the service row %q", id)
	}

	env, err := m.resolveEnv(ctx, "")
	if err != nil || len(env) != 0 {
		t.Errorf("resolving a machine with no service returned (%v, %v)", env, err)
	}
}

// A machine created with only a command still needs somewhere to be grouped
// from later, but nothing to store now.
func TestAnAppAloneStillRecordsAService(t *testing.T) {
	ctx := context.Background()
	m, store := envManager(t, true)

	id, err := m.provisionService(ctx, "api", createEnv{App: "shop"})
	if err != nil {
		t.Fatal(err)
	}
	if id == "" {
		t.Fatal("a machine in an app got no service row")
	}
	svc, err := store.GetService(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if svc.App != "shop" || svc.Replicas != 1 {
		t.Errorf("service row is %+v", svc)
	}
}

// A destroyed machine must not leave its sealed environment behind.
//
// provisionService mints a service row per create, and Destroy never removed
// it. The row carries the machine's sealed secrets, so every destroyed
// machine left a secret replicated to every host in the fleet, indefinitely,
// for a machine that no longer exists.
func TestReleasingTheLastMachineDeletesItsService(t *testing.T) {
	ctx := context.Background()
	m, store := envManager(t, true)

	svcID, err := m.provisionService(ctx, "web", createEnv{App: "shop", SecretEnv: map[string]string{"API_KEY": "sekrit"}})
	if err != nil {
		t.Fatalf("provisionService: %v", err)
	}
	row := &state.Machine{ID: "m-1", HostID: "host-a", ServiceID: svcID, State: "running"}
	if err := store.PutMachine(ctx, row); err != nil {
		t.Fatalf("PutMachine: %v", err)
	}
	if err := store.DeleteMachine(ctx, row.ID); err != nil {
		t.Fatalf("DeleteMachine: %v", err)
	}

	if err := m.releaseService(ctx, row); err != nil {
		t.Fatalf("releaseService: %v", err)
	}
	if _, err := store.GetService(ctx, svcID); !errors.Is(err, state.ErrNotFound) {
		t.Errorf("the service row survived its last machine: err=%v", err)
	}
}

// The volume binding goes with the service row, and the volume itself does
// not. A binding left behind refuses that volume to the next service that
// names it, forever, for a service nobody can see.
func TestReleaseServiceDropsTheVolumeBinding(t *testing.T) {
	ctx := context.Background()
	m, store := envManager(t, true)

	svcID, err := m.provisionService(ctx, "db", createEnv{App: "shop"})
	if err != nil {
		t.Fatalf("provisionService: %v", err)
	}
	if err := store.PutServiceVolume(ctx,
		&state.ServiceVolume{ServiceID: svcID, Ordinal: 1, VolumeID: "vol-1"}); err != nil {
		t.Fatalf("PutServiceVolume: %v", err)
	}
	if err := store.PutVolume(ctx, &state.Volume{ID: "vol-1", Name: "data", HostID: "host-a"}); err != nil {
		t.Fatalf("PutVolume: %v", err)
	}
	row := &state.Machine{ID: "m-1", HostID: "host-a", ServiceID: svcID, VolumeID: "vol-1", State: "running"}
	if err := store.PutMachine(ctx, row); err != nil {
		t.Fatalf("PutMachine: %v", err)
	}
	if err := store.DeleteMachine(ctx, row.ID); err != nil {
		t.Fatalf("DeleteMachine: %v", err)
	}

	if err := m.releaseService(ctx, row); err != nil {
		t.Fatalf("releaseService: %v", err)
	}
	if _, err := store.GetService(ctx, svcID); !errors.Is(err, state.ErrNotFound) {
		t.Errorf("the service row survived its last machine: err=%v", err)
	}
	if _, err := store.ServiceVolume(ctx, svcID); !errors.Is(err, state.ErrNotFound) {
		t.Errorf("the volume binding survived its service: err=%v", err)
	}
	if _, err := store.GetVolume(ctx, "vol-1"); err != nil {
		t.Errorf("the volume itself was deleted with the service: %v", err)
	}
}

// And a service still carrying machines must survive, or destroying one
// replica takes the environment away from its siblings.
func TestAServiceWithMachinesLeftIsKept(t *testing.T) {
	ctx := context.Background()
	m, store := envManager(t, true)

	svcID, err := m.provisionService(ctx, "web", createEnv{App: "shop", SecretEnv: map[string]string{"API_KEY": "sekrit"}})
	if err != nil {
		t.Fatalf("provisionService: %v", err)
	}
	going := &state.Machine{ID: "m-1", HostID: "host-a", ServiceID: svcID, State: "running"}
	staying := &state.Machine{ID: "m-2", HostID: "host-a", ServiceID: svcID, State: "running"}
	for _, row := range []*state.Machine{going, staying} {
		if err := store.PutMachine(ctx, row); err != nil {
			t.Fatalf("PutMachine %s: %v", row.ID, err)
		}
	}
	if err := store.DeleteMachine(ctx, going.ID); err != nil {
		t.Fatalf("DeleteMachine: %v", err)
	}

	if err := m.releaseService(ctx, going); err != nil {
		t.Fatalf("releaseService: %v", err)
	}
	if _, err := store.GetService(ctx, svcID); err != nil {
		t.Errorf("the service was deleted while %s still uses it: %v", staying.ID, err)
	}
}
