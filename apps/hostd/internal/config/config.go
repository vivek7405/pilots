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
	// GuestAgentBin is the agent injected into every image a build produces.
	// Without it a built machine boots and is unreachable: exec, the clock
	// poke and the port proxy all go through the agent.
	GuestAgentBin string // PILOT_GUEST_AGENT
	BuildCacheDir string // PILOT_BUILD_CACHE
	// BuildkitSock addresses the rootless buildkitd, which runs as a DIFFERENT
	// user from hostd on purpose: a build runs an arbitrary user Dockerfile on a
	// host that also runs other tenants' machines. So its socket lives under
	// that user's runtime directory and hostd cannot derive the path from its
	// own uid. host-bootstrap.sh writes it.
	BuildkitSock string // PILOT_BUILDKIT_SOCK
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

	// DashboardDomain is the apex the dashboard is served on, and NOTHING
	// else. Kept apart from WorkloadDomain on purpose (see above); named here
	// only because the router has to manage a certificate for it.
	DashboardDomain string // PILOT_DASHBOARD_DOMAIN

	// ACMEEmail is the contact Let's Encrypt requires. Empty turns TLS off
	// entirely rather than issuing without one: a fleet that cannot be
	// notified about expiring certificates should not be holding any.
	ACMEEmail string // PILOT_ACME_EMAIL

	// CloudflareAPIToken authorises the ACME DNS-01 challenge, which is the
	// only way to obtain the *.<workload domain> wildcard: HTTP-01 cannot
	// issue a wildcard at all. Empty leaves the router HTTP-01-only, which
	// still serves custom domains on demand and simply has no wildcard --
	// a degradation rather than a failure, logged once at startup.
	CloudflareAPIToken string // PILOT_CLOUDFLARE_API_TOKEN

	// The GitHub App, for push-to-deploy and pull-request previews. All three
	// empty turns the webhook route off rather than half-configuring it: a
	// route that accepts deliveries it cannot verify is worse than no route.
	GitHubAppID      int64  // PILOT_GITHUB_APP_ID
	GitHubKeyPath    string // PILOT_GITHUB_APP_KEY
	GitHubWebhookKey string // PILOT_GITHUB_WEBHOOK_SECRET

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

	// FleetKey seals secret environment values before they are written to a
	// replicated row, and it is the one piece of state whose durability is the
	// operator's job.
	//
	// A stated exception to "object storage is the only truth": it is
	// operator-held, supplied out of band to host-bootstrap.sh, and lives only
	// in /etc/pilots/config. Wipe every host and the sealed values are
	// unrecoverable with object storage completely intact. Rotating it means a
	// re-seal sweep over every affected row.
	FleetKey string // PILOT_FLEET_KEY

	// DNSUpstream is where queries that are not under .internal go, as a
	// comma-separated list. Forwarded from the ROOT namespace, so these are
	// resolvers the HOST can reach rather than ones a guest could.
	DNSUpstream string // PILOT_DNS_UPSTREAM

	// AgentTokenSecret is what per-machine guest credentials are DERIVED
	// from, fleet-wide. It is what lets a machine be rescued and still be
	// reachable: the host that takes it over has never held that machine's
	// token, and the hash on its row cannot authenticate to the guest.
	AgentTokenSecret string // PILOT_AGENT_TOKEN_SECRET
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

		KernelPath:      env("PILOT_KERNEL", "/opt/pilots/kernels/vmlinux-6.1.158/vmlinux.bin"),
		FirecrackerBin:  env("PILOT_FIRECRACKER", "/opt/pilots/bin/firecracker"),
		JailerBin:       env("PILOT_JAILER", "/opt/pilots/bin/jailer"),
		TemplateRootfs:  env("PILOT_TEMPLATE_ROOTFS", "/var/lib/pilots/templates/golden.ext4"),
		GuestAgentBin:   env("PILOT_GUEST_AGENT", "/opt/pilots/bin/guest-agent"),
		BuildCacheDir:   env("PILOT_BUILD_CACHE", "/var/cache/pilot-build"),
		BuildkitSock:    os.Getenv("PILOT_BUILDKIT_SOCK"),
		ChrootBase:      env("PILOT_CHROOT_BASE", "/var/lib/pilots/jailer"),
		CPUTemplate:     os.Getenv("PILOT_CPU_TEMPLATE"),
		JailUID:         envInt("PILOT_JAIL_UID", 0),
		JailGID:         envInt("PILOT_JAIL_GID", 0),
		WorkloadDomain:  env("PILOT_WORKLOAD_DOMAIN", "pilotrun.app"),
		DashboardDomain: env("PILOT_DASHBOARD_DOMAIN", "pilots.run"),
		ACMEEmail:       env("PILOT_ACME_EMAIL", ""),

		CloudflareAPIToken: os.Getenv("PILOT_CLOUDFLARE_API_TOKEN"),

		GitHubAppID:      int64(envInt("PILOT_GITHUB_APP_ID", 0)),
		GitHubKeyPath:    env("PILOT_GITHUB_APP_KEY", ""),
		GitHubWebhookKey: env("PILOT_GITHUB_WEBHOOK_SECRET", ""),

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

		FleetKey:         os.Getenv("PILOT_FLEET_KEY"),
		DNSUpstream:      env("PILOT_DNS_UPSTREAM", "1.1.1.1:53,8.8.8.8:53"),
		AgentTokenSecret: os.Getenv("PILOT_AGENT_TOKEN_SECRET"),
	}

	if c.HostID == "" {
		return nil, fmt.Errorf("PILOT_HOST_ID is unset and the hostname is empty")
	}
	if c.Fleet() && !c.MeshEnabled {
		// Refused rather than tolerated: such a host joins the replicated
		// store and creates machines there, but never heartbeats and never
		// runs the self-heal loops. Its peers judge it dead and rescue
		// machines it is still serving -- silent dual-run, the exact failure
		// the fleet exists to prevent.
		return nil, fmt.Errorf("PILOT_STATE_BACKEND=corrosion requires PILOT_MESH_ENABLED=1: " +
			"a fleet host that is not on the mesh never heartbeats, so its peers " +
			"will judge it dead and rescue machines it is still serving")
	}
	return c, nil
}
