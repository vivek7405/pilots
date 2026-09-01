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
	"time"
)

const defaultAgentPort = "3001"

// tokenPath holds this machine's agent token. Every machine gets its own at
// create time -- a token shared across the fleet would let any guest speak for
// any other.
const tokenPath = "/etc/pilot-agent/token"

var agentToken string

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)

	raw, err := os.ReadFile(tokenPath)
	if err != nil {
		log.Fatalf("guest-agent: read %s: %v", tokenPath, err)
	}
	agentToken = strings.TrimSpace(string(raw))
	if agentToken == "" {
		log.Fatalf("guest-agent: %s is empty; refusing to serve unauthenticated", tokenPath)
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
		log.Fatalf("guest-agent: %v", err)
	}
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
	return subtle.ConstantTimeCompare([]byte(presented), []byte(agentToken)) == 1
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
