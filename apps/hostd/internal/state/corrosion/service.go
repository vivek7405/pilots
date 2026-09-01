package corrosion

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/template"
	"time"
)

// Corrosion runs as its own systemd unit, not inside hostd and not in a
// container: three processes per host, systemd managing two of them. This file
// renders what that unit needs and tells hostd when the agent is actually
// usable.

// GossipPort and APIPort are fleet-wide constants. They are not configurable
// on purpose -- a host whose gossip port differs from its peers' is a host
// that silently never joins.
const (
	GossipPort = 51001
	APIPort    = 51002

	// GossipMaxMTU is pinned from the SMALLEST WireGuard MTU any host could
	// have, not this host's actual one.
	//
	// Gossip is QUIC inside WireGuard over an IPv6 ULA. Left to discover the
	// path MTU itself, QUIC overestimates across a heterogeneous underlay and
	// gossip black-holes -- which presents as the cluster flapping at random,
	// nowhere near the MTU. 1280 is the IPv6 minimum link MTU, less 40 bytes
	// of IPv6 header and 8 of UDP. Must stay at or above 1200.
	GossipMaxMTU = 1232
)

// ServiceConfig describes this host's agent.
type ServiceConfig struct {
	// Dir holds the agent's database, schema and rendered config.
	Dir string
	// RunDir holds the admin socket.
	RunDir string
	// MeshAddr is this host's WireGuard address, which gossip binds to. Gossip
	// rides the mesh, so this is never a public address.
	MeshAddr string
	// Bootstrap are peers' mesh addresses. Empty on the first host, which
	// bootstraps a cluster of one.
	Bootstrap []string
	// BearerToken authenticates API callers. Shared cluster-wide.
	BearerToken string
}

// configTemplate is corrosion's config.toml.
var configTemplate = template.Must(template.New("corrosion").Parse(
	`# Rendered by hostd. Edits are overwritten on the next start.

[db]
path = "{{.DBPath}}"
schema_paths = ["{{.SchemaPath}}"]

[gossip]
addr = "[{{.MeshAddr}}]:{{.GossipPort}}"
bootstrap = [{{range $i, $p := .Bootstrap}}{{if $i}}, {{end}}"[{{$p}}]:{{$.GossipPort}}"{{end}}]
# Pinned from the smallest MTU any host could have; see GossipMaxMTU.
max_mtu = {{.MaxMTU}}
# The mesh already authenticates and encrypts every byte. TLS on top of it
# buys nothing and costs a second handshake.
plaintext = true

[api]
addr = "127.0.0.1:{{.APIPort}}"

[api.authz]
bearer-token = "{{.BearerToken}}"

[admin]
path = "{{.AdminSocket}}"
`))

// Paths returns where the agent's files live.
func (c ServiceConfig) DBPath() string      { return filepath.Join(c.Dir, "store.db") }
func (c ServiceConfig) SchemaPath() string  { return filepath.Join(c.Dir, "schema.sql") }
func (c ServiceConfig) ConfigPath() string  { return filepath.Join(c.Dir, "config.toml") }
func (c ServiceConfig) AdminSocket() string { return filepath.Join(c.RunDir, "admin.sock") }

// Render writes config.toml and schema.sql for the agent.
//
// The schema is written as a FILE rather than executed, because corrosion does
// not replicate DDL: schema comes from schema_paths at startup and must be
// byte-identical on every host. A CREATE TABLE sent through the transaction
// API applies locally and silently diverges the cluster.
func (c ServiceConfig) Render(schema string) error {
	if err := os.MkdirAll(c.Dir, 0o755); err != nil {
		return fmt.Errorf("corrosion: mkdir %s: %w", c.Dir, err)
	}
	if err := os.MkdirAll(c.RunDir, 0o755); err != nil {
		return fmt.Errorf("corrosion: mkdir %s: %w", c.RunDir, err)
	}
	if err := os.WriteFile(c.SchemaPath(), []byte(schema), 0o644); err != nil {
		return fmt.Errorf("corrosion: write schema: %w", err)
	}

	var out strings.Builder
	if err := configTemplate.Execute(&out, struct {
		ServiceConfig
		DBPath      string
		SchemaPath  string
		AdminSocket string
		GossipPort  int
		APIPort     int
		MaxMTU      int
	}{
		ServiceConfig: c,
		DBPath:        c.DBPath(), SchemaPath: c.SchemaPath(), AdminSocket: c.AdminSocket(),
		GossipPort: GossipPort, APIPort: APIPort, MaxMTU: GossipMaxMTU,
	}); err != nil {
		return fmt.Errorf("corrosion: render config: %w", err)
	}

	if err := os.WriteFile(c.ConfigPath(), []byte(out.String()), 0o644); err != nil {
		return fmt.Errorf("corrosion: write config: %w", err)
	}
	return nil
}

// WaitReady blocks until the agent can answer a query against the schema.
//
// Not "until the port is open": corrosion serves its API BEFORE it applies
// schema_paths, so a client that starts as soon as it can connect gets "no
// such table" for every read until the schema lands. Probing an actual table
// is what distinguishes listening from usable.
func WaitReady(ctx context.Context, client *Client, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	wait := 100 * time.Millisecond
	var last error

	for time.Now().Before(deadline) {
		rows, err := client.Query(ctx, `SELECT 1 FROM hosts LIMIT 1`)
		if err == nil {
			for rows.Next() {
			}
			err = rows.Err()
			rows.Close()
		}
		if err == nil {
			return nil
		}
		last = err

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
		if wait *= 2; wait > time.Second {
			wait = time.Second
		}
	}
	return fmt.Errorf("corrosion: agent was not ready within %s: %w", timeout, last)
}
