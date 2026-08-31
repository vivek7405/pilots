// Package config loads hostd's runtime configuration from the environment.
//
// Every knob has a default that works on a freshly bootstrapped host, so a
// host needs only PILOT_PUBLIC_IP and the S3 credentials to be useful.
package config

import (
	"fmt"
	"os"
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
	ChrootBase string // PILOT_CHROOT_BASE

	// Object storage: the only truth for machine state. Local disk is cache.
	S3Endpoint  string // PILOT_S3_ENDPOINT
	S3Region    string // PILOT_S3_REGION
	S3Bucket    string // PILOT_S3_BUCKET
	S3AccessKey string // PILOT_S3_ACCESS_KEY
	S3SecretKey string // PILOT_S3_SECRET_KEY
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

		S3Endpoint:  os.Getenv("PILOT_S3_ENDPOINT"),
		S3Region:    env("PILOT_S3_REGION", "eu-central-1"),
		S3Bucket:    os.Getenv("PILOT_S3_BUCKET"),
		S3AccessKey: os.Getenv("PILOT_S3_ACCESS_KEY"),
		S3SecretKey: os.Getenv("PILOT_S3_SECRET_KEY"),
	}

	if c.HostID == "" {
		return nil, fmt.Errorf("PILOT_HOST_ID is unset and the hostname is empty")
	}
	return c, nil
}
