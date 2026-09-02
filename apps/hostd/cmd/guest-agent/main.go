// Command guest-agent runs inside every microVM.
//
// It is baked into the golden rootfs and started by systemd on boot, so it
// builds fully static (CGO_ENABLED=0): the guest has no toolchain and no
// shared libraries we control.
//
// It is the only way in. hostd reaches it over the constant link-local address
// every guest shares, and the router proxies application traffic through it.
package main

import (
	"crypto/subtle"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"time"
)

const defaultAgentPort = "3001"

// tokenPath holds this machine's agent token. Every machine gets its own at
// create time -- a token shared across the fleet would let any guest speak for
// any other.
const tokenPath = "/etc/pilot-agent/token"

// tokenFile is where the token is actually read from. A variable rather than
// the constant so the reload path can be exercised against a temporary file;
// nothing in the guest ever changes it.
var tokenFile = tokenPath

// agentToken is the current credential. Held in an atomic rather than a plain
// string because it can be REPLACED while the agent runs: the create path
// writes a fresh token into the file and the agent has to start accepting it.
//
// The golden rootfs restarts the agent under systemd to pick the new one up.
// An image built from a Dockerfile usually has no systemd to restart it with,
// so the reload happens here instead -- see authOK.
var agentToken atomic.Value // string

func currentToken() string {
	tok, _ := agentToken.Load().(string)
	return tok
}

// reloadToken re-reads the token file, returning what it now holds.
func reloadToken() string {
	raw, err := os.ReadFile(tokenFile)
	if err != nil {
		return currentToken()
	}
	tok := strings.TrimSpace(string(raw))
	if tok == "" {
		return currentToken()
	}
	agentToken.Store(tok)
	return tok
}

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)

	// An image built from a Dockerfile usually carries no init system, so the
	// build path makes this binary /sbin/init. Before that first read of the
	// token there is no /proc, no writable root and no /dev -- so this comes
	// first, and it is a no-op in the golden rootfs where systemd is PID 1.
	if isPID1() {
		runAsInit()
	}

	// The guest's own addressing, and NOT under isPID1.
	//
	// A built image that carries systemd gets /sbin/init pointed at systemd by
	// the build, and this agent runs as a unit instead of as init -- so gating
	// this on PID 1 fixed alpine and left `FROM ubuntu:24.04` with no fdee::21
	// and no route to its peers, which is the identical silent failure the
	// PID-1 path exists to prevent. The build writes no .network file either;
	// the golden rootfs's comes from scripts/rootfs/eth0.network, which a
	// Dockerfile build never sees.
	//
	// Safe to run in both cases because it is idempotent: an address or route
	// already configured (by systemd-networkd, or by a previous restart of
	// this unit) reports EEXIST and is treated as success.
	configureNetwork()

	raw, err := os.ReadFile(tokenFile)
	if err != nil {
		die("read %s: %v", tokenFile, err)
	}
	agentToken.Store(strings.TrimSpace(string(raw)))
	if currentToken() == "" {
		die("%s is empty; refusing to serve unauthenticated", tokenPath)
	}

	port := os.Getenv("AGENT_PORT")
	if port == "" {
		port = defaultAgentPort
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", handleHealth)
	mux.HandleFunc("POST /init", requireAuth(handleInit))
	mux.HandleFunc("POST /volume", requireAuth(handleVolume))
	mux.HandleFunc("POST /exec", requireAuth(handleExec))
	mux.HandleFunc("GET /exec/stream", requireAuth(handleExecStream))
	mux.HandleFunc("GET /terminal", requireAuth(handleTerminal))

	srv := &http.Server{
		Addr:              ":" + port,
		Handler:           withPortProxy(mux),
		ReadHeaderTimeout: 10 * time.Second,
		// No WriteTimeout: exec streams and terminals are long-lived.
	}

	log.Printf("guest-agent listening on :%s", port)
	if err := srv.ListenAndServe(); err != nil {
		die("%v", err)
	}
}

// die reports a fatal condition and stops.
//
// It does NOT exit when this process is the guest's init. PID 1 exiting is a
// kernel panic, the boot arguments say panic=1, and the machine reboots into
// the same failure -- so the reason scrolls past on a console nobody is
// watching and the guest boot-loops. Parking instead leaves the message on
// the serial log, which is what /v1/machines/:id/logs returns.
func die(format string, args ...any) {
	log.Printf("guest-agent: "+format, args...)
	if isPID1() {
		log.Printf("guest-agent: this process is the guest's init; parking " +
			"rather than panicking the kernel")
		select {}
	}
	os.Exit(1)
}

func handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "ts": time.Now().UnixNano()})
}

// requireAuth accepts the token in an Authorization header or a query
// parameter. The query form exists because browsers cannot set headers on a
// WebSocket handshake.
func requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !authOK(r) {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		next(w, r)
	}
}

func authOK(r *http.Request) bool {
	presented := r.URL.Query().Get("token")
	if h := r.Header.Get("Authorization"); h != "" {
		if scheme, tok, found := strings.Cut(h, " "); found && strings.EqualFold(scheme, "bearer") {
			presented = tok
		}
	}
	if presented == "" {
		return false
	}
	if subtle.ConstantTimeCompare([]byte(presented), []byte(currentToken())) == 1 {
		return true
	}
	// A miss might mean the token was just replaced. The create path writes
	// the machine's own credential into the file and, in an image with
	// systemd, restarts the agent to pick it up -- but an image built from an
	// ordinary Dockerfile has no systemd to restart anything, and hostd would
	// then hold a token this process never learned about. Re-reading on a miss
	// costs one small file read on a request that was going to be rejected.
	return subtle.ConstantTimeCompare([]byte(presented), []byte(reloadToken())) == 1
}

// withPortProxy forwards any request carrying X-Pilot-Proxy-Port to a local
// port, so the platform can expose arbitrary ports inside the guest without
// the workload knowing anything about the platform.
//
// Deliberately NOT authenticated: this is how end-user traffic reaches the
// application, and the edge has already decided the request may arrive. Only
// the agent's own control endpoints require the token.
func withPortProxy(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if port := r.Header.Get(headerProxyPort); port != "" {
			proxyToLocalPort(w, r, port)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
