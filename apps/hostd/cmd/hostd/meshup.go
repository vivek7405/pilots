package main

import (
	"fmt"
	"log/slog"
	"os"

	"github.com/vivek7405/pilots/hostd/internal/config"
	"github.com/vivek7405/pilots/hostd/internal/mesh"
)

// MeshUpCommand brings the WireGuard interface up and exits.
//
// It exists for an ordering constraint that is otherwise invisible: gossip
// rides the mesh, so corrosion cannot reach its bootstrap peer until the
// tunnel exists -- and corrosion starting first sits retrying an address with
// no route to it, which looks like a peer that is down rather than a link that
// is missing.
//
// So the unit ordering is mesh-up, then corrosion, then hostd. This runs as a
// oneshot before corrosion, and hostd's own startup is idempotent over it.
const MeshUpCommand = "mesh-up"

// MeshAddrCommand prints this host's mesh address and exits.
//
// The bootstrap script needs it to render corrosion's gossip address, and this
// is where it asks. Deriving it a second time in shell would be a second
// implementation of the same rule, and the two disagreeing is not a loud
// failure: corrosion binds one address while the mesh answers on another, and
// gossip simply never arrives.
const MeshAddrCommand = "mesh-addr"

func runMeshAddr() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	keys, err := mesh.LoadOrCreateKeys(cfg.MeshKeyPath())
	if err != nil {
		return err
	}
	fmt.Println(keys.Address())
	return nil
}

func runMeshUp() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	keys, err := mesh.LoadOrCreateKeys(cfg.MeshKeyPath())
	if err != nil {
		return err
	}
	dev, err := mesh.Open(keys)
	if err != nil {
		return fmt.Errorf("bring up the mesh: %w", err)
	}
	defer dev.Close()

	// No peers yet: they come from the hosts table, which needs corrosion,
	// which needs this. Peers are reconciled by hostd once it is running.
	fmt.Printf("mesh %s up at %s\n", mesh.LinkName, dev.Address())
	return nil
}

// dispatchMeshUp runs the subcommand if that is what was asked for.
func dispatchMeshUp() bool {
	if len(os.Args) < 2 {
		return false
	}
	var run func() error
	switch os.Args[1] {
	case MeshUpCommand:
		run = runMeshUp
	case MeshAddrCommand:
		run = runMeshAddr
	default:
		return false
	}
	if err := run(); err != nil {
		slog.Error("command failed", "command", os.Args[1], "err", err)
		os.Exit(1)
	}
	return true
}
