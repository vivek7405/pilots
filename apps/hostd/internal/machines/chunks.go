package machines

import (
	"log/slog"
	"os"
	"strings"
	"sync"

	"github.com/google/uuid"

	"github.com/vivek7405/pilots/hostd/internal/block"
	"github.com/vivek7405/pilots/hostd/internal/chunkserve"
	"github.com/vivek7405/pilots/hostd/internal/fc"
)

// Per-machine chunk sockets, so no handler holds a storage credential.
//
// One server per running machine, listening inside that machine's own state
// directory, answering only for the build ids that machine was started with.
// The manager owns the lifetime: started just before the handlers, closed when
// the machine goes away.
//
// See internal/chunkserve for why this exists rather than passing the bucket
// credentials down.

// chunkServers holds the live servers, keyed by machine id.
type chunkServers struct {
	mu      sync.Mutex
	servers map[string]*chunkserve.Server
}

func newChunkServers() *chunkServers {
	return &chunkServers{servers: map[string]*chunkserve.Server{}}
}

// start brings up a machine's chunk socket and returns its path.
//
// An empty path is not a failure: it means this host has no object store, so
// the handlers read nothing remote and need no socket. A server that cannot be
// started is logged and also answers empty, which falls the handlers back to
// reading storage directly rather than refusing to start the machine.
// Hardening must never be the reason a machine will not boot.
func (c *chunkServers) start(machineID, stateDir string, store block.ObjectStore, allowed []string) string {
	if store == nil || len(allowed) == 0 {
		return ""
	}
	path := chunkserve.SocketPath(stateDir)
	srv, err := chunkserve.New(machineID, path, store, allowed)
	if err != nil {
		slog.Warn("could not start this machine's chunk socket; its handlers "+
			"will read object storage directly", "machine", machineID, "err", err)
		return ""
	}
	c.mu.Lock()
	if old := c.servers[machineID]; old != nil {
		_ = old.Close()
	}
	c.servers[machineID] = srv
	c.mu.Unlock()
	return path
}

// allowedBuilds lists every build id a machine's handlers may read.
//
// Exactly the four the spawn already names: the memory build, its parent, the
// rootfs template, and the rehydrate build. Anything else is a request that
// machine has no reason to make.
func allowedBuilds(cfg fc.InstantConfig) []string {
	ids := []string{
		idString(cfg.MemBuildID),
		idString(cfg.MemParentBuildID),
		idString(cfg.RootfsTemplateID),
		idString(cfg.RootfsDiffID),
	}
	out := ids[:0]
	for _, id := range ids {
		if id != "" {
			out = append(out, id)
		}
	}
	return out
}

func idString(id uuid.UUID) string {
	if id == uuid.Nil {
		return ""
	}
	return id.String()
}

// handlerEnv is the environment a handler process gets.
//
// An allowlist, not hostd's own environment. A handler's job is to read a
// handful of named objects for one machine, and it now does that through the
// chunk socket, so the only thing it needs from the environment is a PATH and
// the few variables the Go runtime itself reads. Everything else, and
// PILOT_S3_ACCESS_KEY above all, was there only because os.Environ() is one
// call and an allowlist is a list.
//
// TMPDIR is included because the block layer writes its cache through
// os.CreateTemp; without it a host with a small /tmp would fail reads for a
// reason that names nothing.
// HandlerEnv is exported for cmd/hostd, which builds the manager options.
func HandlerEnv() []string {
	const keep = "PATH,HOME,TMPDIR,LANG,LC_ALL,SSL_CERT_FILE,SSL_CERT_DIR," +
		"GOMAXPROCS,GOMEMLIMIT,GODEBUG,GOTRACEBACK"
	allow := map[string]bool{}
	for _, k := range strings.Split(keep, ",") {
		allow[k] = true
	}
	var out []string
	for _, kv := range os.Environ() {
		k, _, ok := strings.Cut(kv, "=")
		if ok && allow[k] {
			out = append(out, kv)
		}
	}
	if len(out) == 0 {
		// A handler with no PATH at all cannot exec anything it might need.
		out = append(out, "PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin")
	}
	return out
}

// close stops a machine's chunk socket.
func (c *chunkServers) close(machineID string) {
	c.mu.Lock()
	srv := c.servers[machineID]
	delete(c.servers, machineID)
	c.mu.Unlock()
	if srv == nil {
		return
	}
	if n := srv.Refused(); n > 0 {
		// Said out loud on the way out, because a handler that asked for
		// another machine's build is the one event this whole mechanism
		// exists to make visible.
		slog.Warn("a chunk handler was refused during this machine's life",
			"machine", machineID, "refusals", n)
	}
	_ = srv.Close()
}
