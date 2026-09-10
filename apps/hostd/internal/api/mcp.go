package api

import (
	"fmt"
	"net"
	"net/http"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	pilotsmcp "github.com/vivek7405/pilots/agents/mcp"
	pilots "github.com/vivek7405/pilots/sdks/go"
)

// The hosted MCP endpoint: the fleet toolset (agents/mcp) over Streamable
// HTTP at /mcp, served by every host from the same binary, so an agent with
// no `pilot` binary points its client at the API hostname and is done.
//
// Two rules keep it inside the architecture:
//
//   - STATELESS. The transport keeps no session, so the next request may land
//     on another host behind the same name and nothing is lost. A stateful
//     session would be state that one host holds and the others do not, which
//     is exactly what rule 1 forbids.
//   - EVERY TOOL CALLS BACK THROUGH THE PUBLIC API. The server built for a
//     request holds a client for this host's own listener carrying the
//     caller's bearer key, so authentication, scopes, tenancy narrowing and
//     owner-host forwarding are the paths every other client takes. Nothing
//     here reads the store or the manager directly.
func (d Deps) mcpHandler() http.Handler {
	return mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server {
		opts := []pilots.Option{pilots.WithBaseURL(d.SelfURL)}
		// An admin key acting as one org carries ?org= on the MCP URL, the
		// same way it would on any other call.
		if org := r.URL.Query().Get("org"); org != "" {
			opts = append(opts, pilots.WithOrg(org))
		}
		client := pilots.New(BearerToken(r.Context()), opts...)
		return pilotsmcp.NewServer(client, pilotsmcp.Options{})
	}, &mcp.StreamableHTTPOptions{Stateless: true})
}

// handleProtectedResource is RFC 9728: the document an MCP client fetches
// after a 401 to learn which authorization server issues tokens for this
// resource. The answer names the dashboard, the one place that turns a
// GitHub login into a pilots key; the key it mints IS the bearer token, so
// nothing on the data plane learns a second credential type. Unauthenticated
// and CORS-open, because a browser-based client reads it before it has
// anything to present.
func (d Deps) handleProtectedResource(w http.ResponseWriter, r *http.Request) {
	doc := map[string]any{
		"resource":                 d.apiBase() + "/mcp",
		"resource_name":            "pilots",
		"bearer_methods_supported": []string{"header"},
		"scopes_supported":         []string{ScopeMachines, ScopeDeploy, ScopeAdmin},
		"resource_documentation":   "https://pilots.run/agents",
	}
	if d.DashboardURL != "" {
		doc["authorization_servers"] = []string{d.DashboardURL}
	}
	w.Header().Set("Access-Control-Allow-Origin", "*")
	writeJSON(w, http.StatusOK, doc)
}

// apiBase is this fleet's public API origin, as a client would dial it.
func (d Deps) apiBase() string {
	host := d.APIHostname
	if host == "" {
		host = "api." + d.Domain
	}
	return d.URL.Of(host)
}

// challenge is the WWW-Authenticate value a 401 carries. On /mcp it names the
// protected-resource document, which is how an MCP client discovers the
// login flow without being told the URL; everywhere else the realm alone,
// as before.
func (d Deps) challenge(r *http.Request) string {
	if r.URL.Path != "/mcp" {
		return `Bearer realm="pilots"`
	}
	return fmt.Sprintf(`Bearer realm="pilots", resource_metadata=%q`, d.apiBase()+"/.well-known/oauth-protected-resource")
}

// LoopbackURL turns the listen address into the URL this process reaches
// itself at: the plain listener, on the loopback interface. A wildcard or
// empty host becomes 127.0.0.1; a named one is kept.
func LoopbackURL(listen string) string {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return "http://127.0.0.1" + listen
	}
	switch host {
	case "", "0.0.0.0", "::", "[::]":
		host = "127.0.0.1"
	}
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	return "http://" + host + ":" + port
}
