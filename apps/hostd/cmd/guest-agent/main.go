// Command guest-agent runs inside every microVM.
//
// It is baked into the golden rootfs and started by systemd on boot, so it
// must build fully static (CGO_ENABLED=0) -- the guest has no Go toolchain and
// no shared libraries we control.
//
// Phase 1 ships liveness only, which is what the golden rootfs needs in order
// to be built and booted. Phase 2 (issue #3) adds the real surface: /init
// (CLOCK_REALTIME poke after a restore), /exec, the WebSocket /exec/stream
// carrying the sprites-compatible frame protocol, /terminal, token auth, and
// the X-Pilot-Proxy-Port reverse proxy.
package main

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"time"
)

func main() {
	port := os.Getenv("AGENT_PORT")
	if port == "" {
		port = "3001"
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"ok": true})
	})

	// Listens on all interfaces, IPv4 and IPv6: the host reaches the guest over
	// the constant link-local address baked into the rootfs' network config.
	srv := &http.Server{
		Addr:              ":" + port,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	log.Printf("guest-agent listening on :%s", port)
	if err := srv.ListenAndServe(); err != nil {
		log.Fatalf("guest-agent: %v", err)
	}
}
