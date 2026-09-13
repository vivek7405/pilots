// Package broker hands a machine its own credentials, over a socket only that
// machine can reach.
//
// # The identity is the socket
//
// hostd binds one listener per machine, inside that machine's own network
// namespace, on the constant gateway address every guest already has a route
// to. Nothing is presented and nothing is checked, because there is nothing a
// guest could present that a COPY of that guest could not: a secret baked in is
// a secret in every snapshot, fork and restore of it.
//
// A namespace is not a secret that can leak. The guest firewall already allows
// exactly the gateway /30 and nothing else, and a namespace cannot reach
// another namespace's loopback, so "the request arrived here" is a stronger
// statement about who is asking than any token would be.
//
// # Deny by default
//
// With no grant row, both credential routes answer 403. That is not an error
// path, it is the normal state of a machine nobody has granted anything to, and
// it is why a compromised guest with no grant is a guest that can reach nothing
// on the fleet.
//
// # Nothing here writes
//
// Every read is a local store query. The broker cannot change a grant, cannot
// change a machine, and cannot be made to by anything a guest sends, which
// keeps the blast radius of a bug in this package to "a machine learns its own
// id".
package broker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/vivek7405/pilots/hostd/internal/api"
	"github.com/vivek7405/pilots/hostd/internal/netns"
	"github.com/vivek7405/pilots/hostd/internal/state"
)

// Port is where the broker listens inside every machine's namespace. Declared
// in netns with the other constant addresses, so the guest side and the host
// side cannot disagree about it.
const Port = netns.BrokerPort

// Sealer opens the sealed half of a grant. The fleet key, in practice.
type Sealer interface {
	IsSet() bool
	Open(blob string) ([]byte, error)
}

// Tenancy answers which org owns an object.
type Tenancy interface {
	OrgOf(ctx context.Context, id string) (string, bool)
}

// Options are what a broker needs to answer.
type Options struct {
	Store  state.Store
	Seal   Sealer
	Tenant Tenancy
	// Key signs tokens. Empty means this host brokers no tokens, which is a
	// host with no agent-token secret: it still answers /identity, because
	// knowing your own id is not a credential.
	Key []byte
	// APIURL is the address a machine should call. Handed over so a guest does
	// not have to construct it and cannot construct it wrongly.
	APIURL string
}

// Server binds one listener per machine.
type Server struct {
	opts  Options
	mu    sync.Mutex
	bound map[string]*http.Server
}

func New(opts Options) *Server {
	return &Server{opts: opts, bound: map[string]*http.Server{}}
}

// Bind starts serving inside a namespace that has just been built.
//
// Idempotent, in the shape the DNS responder uses: a second Bind for a machine
// already served is a no-op, and a Bind that loses a race closes its own
// listener rather than leaking it.
func (s *Server) Bind(machineID, netnsName string) error {
	s.mu.Lock()
	if _, ok := s.bound[machineID]; ok {
		s.mu.Unlock()
		return nil
	}
	s.mu.Unlock()

	addr := net.JoinHostPort(netns.TapHostIP, strconv.Itoa(Port))
	var listener net.Listener
	// Opened from inside the namespace, and nothing else is done in there: the
	// fewer instructions run on a thread that is in another namespace, the
	// smaller the window in which a bug strands it.
	if err := netns.Do(netnsName, func() error {
		var err error
		listener, err = net.Listen("tcp", addr)
		return err
	}); err != nil {
		return fmt.Errorf("broker: bind in %s: %w", netnsName, err)
	}

	srv := &http.Server{
		Handler: s.handlerFor(machineID),
		// A guest that opens a connection and says nothing must not hold a
		// goroutine for ever. Short, because every request here is a local
		// store read answered in microseconds.
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
	}

	s.mu.Lock()
	if _, ok := s.bound[machineID]; ok {
		s.mu.Unlock()
		listener.Close()
		return nil
	}
	s.bound[machineID] = srv
	s.mu.Unlock()

	go func() {
		if err := srv.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Warn("the credential broker stopped", "machine", machineID, "err", err)
		}
	}()
	return nil
}

// Release stops serving a machine, before its namespace is torn down.
func (s *Server) Release(machineID string) {
	s.mu.Lock()
	srv, ok := s.bound[machineID]
	delete(s.bound, machineID)
	s.mu.Unlock()
	if !ok {
		return
	}
	_ = srv.Close()
}

// Close releases every binding.
func (s *Server) Close() {
	s.mu.Lock()
	ids := make([]string, 0, len(s.bound))
	for id := range s.bound {
		ids = append(ids, id)
	}
	s.mu.Unlock()
	for _, id := range ids {
		s.Release(id)
	}
}

// IdentityResponse is who a machine is. Not a credential: a machine knowing its
// own id lets it do nothing it could not already do.
type IdentityResponse struct {
	MachineID string `json:"machine_id"`
	OrgID     string `json:"org_id"`
	ServiceID string `json:"service_id,omitempty"`
	APIURL    string `json:"api_url,omitempty"`
}

// TokenResponse is a minted credential and when it dies.
type TokenResponse struct {
	Token     string   `json:"token"`
	ExpiresAt int64    `json:"expires_at"`
	Scopes    []string `json:"scopes"`
}

// SecretsResponse is the values granted to this machine.
type SecretsResponse struct {
	Secrets map[string]string `json:"secrets"`
}

// HandlerFor is the broker's routes for one machine, exported so a test can
// drive them without a namespace.
func (s *Server) HandlerFor(machineID string) http.Handler { return s.handlerFor(machineID) }

func (s *Server) handlerFor(machineID string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /identity", func(w http.ResponseWriter, r *http.Request) {
		s.identity(w, r, machineID)
	})
	mux.HandleFunc("GET /token", func(w http.ResponseWriter, r *http.Request) {
		s.token(w, r, machineID)
	})
	mux.HandleFunc("GET /secrets", func(w http.ResponseWriter, r *http.Request) {
		s.secrets(w, r, machineID)
	})
	return mux
}

func (s *Server) identity(w http.ResponseWriter, r *http.Request, machineID string) {
	m, err := s.opts.Store.GetMachine(r.Context(), machineID)
	if err != nil {
		writeErr(w, http.StatusNotFound, "this machine is not in the local replica")
		return
	}
	org, _ := s.opts.Tenant.OrgOf(r.Context(), machineID)
	writeJSON(w, http.StatusOK, IdentityResponse{
		MachineID: machineID, OrgID: org, ServiceID: m.ServiceID, APIURL: s.opts.APIURL,
	})
}

func (s *Server) token(w http.ResponseWriter, r *http.Request, machineID string) {
	grant, m, ok := s.resolve(w, r, machineID)
	if !ok {
		return
	}
	if len(grant.Scopes) == 0 {
		denied(w, "this machine may hold no token")
		return
	}
	// What the caller asked for, narrowed to what it was granted. Asking for
	// nothing means "everything I was granted", which is what a client with no
	// opinion should get.
	want := grant.Scopes
	if raw := r.URL.Query().Get("scope"); raw != "" {
		want = nil
		granted := map[string]bool{}
		for _, scope := range grant.Scopes {
			granted[scope] = true
		}
		for _, scope := range strings.Split(raw, ",") {
			scope = strings.TrimSpace(scope)
			if scope == "" {
				continue
			}
			if !granted[scope] {
				denied(w, "this machine was not granted "+scope)
				return
			}
			want = append(want, scope)
		}
		if len(want) == 0 {
			denied(w, "no scope asked for")
			return
		}
	}

	token, err := api.MintBrokerToken(s.opts.Key, api.BrokerClaims{
		Org: grant.OrgID, Machine: machineID, Service: m.ServiceID, Scopes: want,
	})
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, TokenResponse{
		Token:     token,
		ExpiresAt: time.Now().Add(api.BrokerTokenLife).Unix(),
		Scopes:    want,
	})
}

func (s *Server) secrets(w http.ResponseWriter, r *http.Request, machineID string) {
	grant, _, ok := s.resolve(w, r, machineID)
	if !ok {
		return
	}
	if grant.Sealed == "" {
		denied(w, "this machine was granted no secrets")
		return
	}
	if s.opts.Seal == nil || !s.opts.Seal.IsSet() {
		writeErr(w, http.StatusServiceUnavailable,
			"this host holds no fleet key, so it cannot open what was granted")
		return
	}
	raw, err := s.opts.Seal.Open(grant.Sealed)
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable,
			"this host's fleet key does not open this grant")
		return
	}
	defer clear(raw)
	var secrets map[string]string
	if err := json.Unmarshal(raw, &secrets); err != nil {
		writeErr(w, http.StatusInternalServerError, "the grant is not readable as a map")
		return
	}
	writeJSON(w, http.StatusOK, SecretsResponse{Secrets: secrets})
}

// resolve finds the grant that applies to a machine.
//
// The MACHINE's own grant first, then its service's. Both, because a service's
// replicas are recreated on every deploy: a grant written per replica could
// never serve the PaaS face, and a grant written only per service could never
// serve a bare sandbox. The machine's own wins where both exist, because it is
// the more specific statement and somebody wrote it deliberately.
func (s *Server) resolve(w http.ResponseWriter, r *http.Request, machineID string) (*state.BrokerGrant, *state.Machine, bool) {
	m, err := s.opts.Store.GetMachine(r.Context(), machineID)
	if err != nil {
		writeErr(w, http.StatusNotFound, "this machine is not in the local replica")
		return nil, nil, false
	}
	// A destroyed machine brokers nothing, checked here as well as at the API,
	// because the namespace can outlive the row by the length of a teardown.
	if m.State == state.StateDestroyed {
		denied(w, "this machine is destroyed")
		return nil, nil, false
	}

	grant, err := s.opts.Store.GetBrokerGrant(r.Context(), machineID)
	if err == nil {
		return grant, m, true
	}
	if !errors.Is(err, state.ErrNotFound) {
		writeErr(w, http.StatusServiceUnavailable, "could not read the grant")
		return nil, nil, false
	}
	if m.ServiceID != "" {
		grant, err = s.opts.Store.GetBrokerGrant(r.Context(), m.ServiceID)
		if err == nil {
			return grant, m, true
		}
		if !errors.Is(err, state.ErrNotFound) {
			writeErr(w, http.StatusServiceUnavailable, "could not read the grant")
			return nil, nil, false
		}
	}
	denied(w, "nothing has been granted to this machine")
	return nil, nil, false
}

// denied is the deny-by-default answer, with one shape so a client can branch
// on it: the absence of a grant and the refusal of a scope are the same class
// of "no", and a client should treat both as "run without credentials".
func denied(w http.ResponseWriter, why string) {
	writeJSON(w, http.StatusForbidden, map[string]string{
		"error": why,
		"code":  "no_grant",
		"next":  "grant it: PUT /v1/machines/{id}/secrets with an API key",
	})
}

func writeErr(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
