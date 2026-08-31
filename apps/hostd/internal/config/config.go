// Package config loads hostd's runtime configuration from the environment.
//
// Every knob has a default that works on a freshly bootstrapped host, so a
// host needs only PILOT_PUBLIC_IP and the S3 credentials to be useful.
package config

import (
	"fmt"
	"os"
	"strconv"
)

// Config is the full runtime configuration for a hostd process.
type Config struct {
	// Server
	ListenAddr string // PILOT_LISTEN
	HostID     string // PILOT_HOST_ID (defaults to the hostname)
	PublicIP   string // PILOT_PUBLIC_IP

	// State. In phase 2 this is a local SQLite file; from phase 4 the same
	// schema is served by Corrosion and this becomes its replica path.
	StateDSN string // PILOT_STATE_DSN

	// Firecracker artifacts, installed by scripts/fetch-*.sh.
	KernelPath     string // PILOT_KERNEL
	FirecrackerBin string // PILOT_FIRECRACKER
	JailerBin      string // PILOT_JAILER
	TemplateRootfs string // PILOT_TEMPLATE_ROOTFS
	BuildCacheDir  string // PILOT_BUILD_CACHE
	// ChrootBase must be on a filesystem that allows device nodes. The jailer
	// creates /dev/{net/tun,kvm,userfaultfd} inside each machine's chroot, and
	// on a nodev mount (any default /tmp) they can be created but never
	// opened -- surfacing much later as an unrelated permission error.
	ChrootBase  string // PILOT_CHROOT_BASE
	CPUTemplate string // PILOT_CPU_TEMPLATE
	JailUID     int    // PILOT_JAIL_UID
	JailGID     int    // PILOT_JAIL_GID

	// WorkloadDomain is the apex every machine's URL sits under. Workloads
	// never share the dashboard's apex: a guest on the same apex could set
	// cookies scoped to it.
	WorkloadDomain string // PILOT_WORKLOAD_DOMAIN

	// Object storage: the only truth for machine state. Local disk is cache.
	S3Endpoint  string // PILOT_S3_ENDPOINT
	S3Region    string // PILOT_S3_REGION
	S3Bucket    string // PILOT_S3_BUCKET
	S3AccessKey string // PILOT_S3_ACCESS_KEY
	S3SecretKey string // PILOT_S3_SECRET_KEY

	// StateBackend selects where cluster state lives: "sqlite" for a single
	// box, "corrosion" for a fleet. Same Store interface behind both, so
	// nothing downstream knows which is in use.
	StateBackend string // PILOT_STATE_BACKEND

	// CorrosionAddr is the local agent's API. Loopback by design: the agent
	// is a sibling process, and its API is not something a peer talks to.
	CorrosionAddr  string // PILOT_CORROSION_ADDR
	CorrosionToken string // PILOT_CORROSION_TOKEN

	// MeshEnabled brings up WireGuard. Off on a single box, where there is
	// nothing to mesh with.
	MeshEnabled bool // PILOT_MESH_ENABLED
	// MeshBootstrap is the peer a joining host was given, as
	// "<public-key>@<host>:<port>". It is the one edge that is configured
	// rather than discovered, and without it a new host can never join: its
	// peers come from the hosts table, the table arrives by gossip, and
	// gossip rides the mesh.
	MeshBootstrap string // PILOT_MESH_BOOTSTRAP
}

// Fleet reports whether this host is part of a cluster.
func (c *Config) Fleet() bool { return c.StateBackend == "corrosion" }

// CorrosionDir holds the agent's database, schema and rendered config.
func (c *Config) CorrosionDir() string { return "/var/lib/pilots/corrosion" }

// CorrosionRunDir holds the agent's admin socket.
func (c *Config) CorrosionRunDir() string { return "/run/pilots/corrosion" }

// MeshKeyPath is this host's mesh identity. It must survive restarts: the
// address is derived from it, so a regenerated key makes this a different host
// to every peer.
func (c *Config) MeshKeyPath() string { return "/var/lib/pilots/mesh.key" }

// MachineStateRoot holds per-machine breadcrumbs. On persistent disk, not
// /var/run: a host reboot must not orphan the bookkeeping for machines whose
// snapshots are still in object storage.
func (c *Config) MachineStateRoot() string { return "/var/lib/pilots/machines" }

// CacheRoot holds snapshot files pulled back from object storage. Purely a
// cache -- deleting it costs a re-download, never data.
func (c *Config) CacheRoot() string { return "/var/cache/pilots" }

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// Load reads the environment and validates what the process cannot run without.
func Load() (*Config, error) {
	host, _ := os.Hostname()

	c := &Config{
		ListenAddr: env("PILOT_LISTEN", ":8080"),
		HostID:     env("PILOT_HOST_ID", host),
		PublicIP:   os.Getenv("PILOT_PUBLIC_IP"),

		StateDSN: env("PILOT_STATE_DSN", "/var/lib/pilots/state.db"),

		KernelPath:     env("PILOT_KERNEL", "/opt/pilots/kernels/vmlinux-6.1.158/vmlinux.bin"),
		FirecrackerBin: env("PILOT_FIRECRACKER", "/opt/pilots/bin/firecracker"),
		JailerBin:      env("PILOT_JAILER", "/opt/pilots/bin/jailer"),
		TemplateRootfs: env("PILOT_TEMPLATE_ROOTFS", "/var/lib/pilots/templates/golden.ext4"),
		BuildCacheDir:  env("PILOT_BUILD_CACHE", "/var/cache/pilot-build"),
		ChrootBase:     env("PILOT_CHROOT_BASE", "/var/lib/pilots/jailer"),
		CPUTemplate:    os.Getenv("PILOT_CPU_TEMPLATE"),
		JailUID:        envInt("PILOT_JAIL_UID", 0),
		JailGID:        envInt("PILOT_JAIL_GID", 0),
		WorkloadDomain: env("PILOT_WORKLOAD_DOMAIN", "pilotrun.app"),

		S3Endpoint:  os.Getenv("PILOT_S3_ENDPOINT"),
		S3Region:    env("PILOT_S3_REGION", "eu-central-1"),
		S3Bucket:    os.Getenv("PILOT_S3_BUCKET"),
		S3AccessKey: os.Getenv("PILOT_S3_ACCESS_KEY"),
		S3SecretKey: os.Getenv("PILOT_S3_SECRET_KEY"),

		StateBackend:   env("PILOT_STATE_BACKEND", "sqlite"),
		CorrosionAddr:  env("PILOT_CORROSION_ADDR", "127.0.0.1:51002"),
		CorrosionToken: os.Getenv("PILOT_CORROSION_TOKEN"),
		MeshEnabled:    os.Getenv("PILOT_MESH_ENABLED") == "1",
		MeshBootstrap:  os.Getenv("PILOT_MESH_BOOTSTRAP"),
	}

	if c.HostID == "" {
		return nil, fmt.Errorf("PILOT_HOST_ID is unset and the hostname is empty")
	}
	return c, nil
}
