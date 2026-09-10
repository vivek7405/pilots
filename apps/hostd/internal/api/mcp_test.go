package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	pilotsmcp "github.com/vivek7405/pilots/agents/mcp"
	"github.com/vivek7405/pilots/hostd/internal/state"
)

// mcpServer is a live HTTP server whose SelfURL is itself, which is exactly
// the production shape: the MCP handler dials the same listener it was
// reached on.
func mcpServer(t *testing.T) (*httptest.Server, state.Store) {
	t.Helper()
	st, err := state.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	sum := sha256.Sum256([]byte(testKey))
	if err := st.PutAPIKey(context.Background(), &state.APIKey{
		Hash: hex.EncodeToString(sum[:]), OrgID: "org_1", Scopes: "admin",
	}); err != nil {
		t.Fatal(err)
	}
	fake := newFakeManager()
	if err := st.PutMachine(context.Background(), fake.machine); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(nil)
	srv.Start()
	t.Cleanup(srv.Close)
	srv.Config.Handler = Routes(Deps{
		HostID: "host-test", Store: st, Machines: fake,
		SelfURL: srv.URL, DashboardURL: "https://dash.test",
		Domain: "pilotrun.test", URL: PublicURL{Scheme: "https"},
	})
	return srv, st
}

type bearerTransport struct{ key string }

func (b bearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r.Header.Set("Authorization", "Bearer "+b.key)
	return http.DefaultTransport.RoundTrip(r)
}

func mcpClient(t *testing.T, srv *httptest.Server, key string) *mcp.ClientSession {
	t.Helper()
	transport := &mcp.StreamableClientTransport{
		Endpoint:   srv.URL + "/mcp",
		HTTPClient: &http.Client{Transport: bearerTransport{key}},
		MaxRetries: -1,
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil).Connect(context.Background(), transport, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

func toolText(r *mcp.CallToolResult) string {
	var b strings.Builder
	for _, c := range r.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}

func TestMCPUnauthenticatedNamesTheResourceMetadata(t *testing.T) {
	srv, _ := mcpServer(t)
	res, err := http.Post(srv.URL+"/mcp", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401", res.StatusCode)
	}
	got := res.Header.Get("WWW-Authenticate")
	want := `Bearer realm="pilots", resource_metadata="https://api.pilotrun.test/.well-known/oauth-protected-resource"`
	if got != want {
		t.Fatalf("WWW-Authenticate = %q, want %q", got, want)
	}
	// Elsewhere the challenge is the plain realm, unchanged.
	res2, err := http.Get(srv.URL + "/v1/machines")
	if err != nil {
		t.Fatal(err)
	}
	defer res2.Body.Close()
	if h := res2.Header.Get("WWW-Authenticate"); h != `Bearer realm="pilots"` {
		t.Fatalf("/v1/machines challenge = %q", h)
	}
}

func TestProtectedResourceDocumentIsPublic(t *testing.T) {
	srv, _ := mcpServer(t)
	for _, path := range []string{"/.well-known/oauth-protected-resource", "/.well-known/oauth-protected-resource/mcp"} {
		res, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		if res.StatusCode != http.StatusOK {
			t.Fatalf("%s: got %d, want 200 with no key", path, res.StatusCode)
		}
		if res.Header.Get("Access-Control-Allow-Origin") != "*" {
			t.Errorf("%s: a browser-based client cannot read it without CORS", path)
		}
		var doc struct {
			Resource string   `json:"resource"`
			Servers  []string `json:"authorization_servers"`
			Methods  []string `json:"bearer_methods_supported"`
		}
		if err := json.NewDecoder(res.Body).Decode(&doc); err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if doc.Resource != "https://api.pilotrun.test/mcp" {
			t.Errorf("resource = %q", doc.Resource)
		}
		if !slices.Equal(doc.Servers, []string{"https://dash.test"}) {
			t.Errorf("authorization_servers = %v", doc.Servers)
		}
		if !slices.Equal(doc.Methods, []string{"header"}) {
			t.Errorf("bearer_methods_supported = %v", doc.Methods)
		}
	}
}

// A fleet with no dashboard names no authorization server rather than a
// URL that does not resolve.
func TestProtectedResourceWithoutADashboard(t *testing.T) {
	h, _ := newTestServer(t)
	rec := do(t, h, "GET", "/.well-known/oauth-protected-resource", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "authorization_servers") {
		t.Fatalf("named a server on a fleet with none: %s", rec.Body.String())
	}
}

func TestMCPStatelessRefusesGET(t *testing.T) {
	srv, _ := mcpServer(t)
	req, _ := http.NewRequest("GET", srv.URL+"/mcp", nil)
	req.Header.Set("Authorization", "Bearer "+testKey)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET /mcp: got %d, want 405 (no session state to stream)", res.StatusCode)
	}
}

func TestMCPListsExactlyTheFleetTools(t *testing.T) {
	srv, _ := mcpServer(t)
	cs := mcpClient(t, srv, testKey)
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(res.Tools))
	for _, tool := range res.Tools {
		names = append(names, tool.Name)
	}
	slices.Sort(names)
	if !slices.Equal(names, pilotsmcp.FleetTools) {
		t.Fatalf("hosted tools drifted:\n got %v\nwant %v", names, pilotsmcp.FleetTools)
	}
	for _, local := range pilotsmcp.LocalTools {
		if slices.Contains(names, local) {
			t.Errorf("%s needs the agent's filesystem and must not be hosted", local)
		}
	}
}

// The tool call goes back through the public API as the caller: the seeded
// machine is visible, and the answer carries `next`.
func TestMCPToolCallsBackThroughTheAPI(t *testing.T) {
	srv, _ := mcpServer(t)
	cs := mcpClient(t, srv, testKey)
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "list_machines", Arguments: map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("list_machines: %s", toolText(res))
	}
	if !strings.Contains(toolText(res), `"id": "m_1"`) || !strings.Contains(toolText(res), `"next"`) {
		t.Fatalf("unexpected body: %s", toolText(res))
	}
	res, err = cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "status", Arguments: map[string]any{"machine": "webapp"}})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError || !strings.Contains(toolText(res), `"name": "webapp"`) {
		t.Fatalf("status by name: %s", toolText(res))
	}
	// The skill rides along as resources.
	rs, err := cs.ListResources(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, r := range rs.Resources {
		if r.URI == "pilots-docs://SKILL.md" {
			found = true
		}
	}
	if !found {
		t.Fatal("SKILL.md is not served as a resource")
	}
}

// Scopes are enforced by the route each tool calls, not by the endpoint: a
// machines key connects and lists machines, and is refused a deploy-scoped
// tool with hostd's own scope_required body.
func TestMCPScopesFlowThroughTheLoopback(t *testing.T) {
	srv, st := mcpServer(t)
	const narrow = "pilot_machines_only"
	sum := sha256.Sum256([]byte(narrow))
	if err := st.PutAPIKey(context.Background(), &state.APIKey{
		Hash: hex.EncodeToString(sum[:]), OrgID: "org_1", Scopes: "machines",
	}); err != nil {
		t.Fatal(err)
	}
	cs := mcpClient(t, srv, narrow)
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "list_machines", Arguments: map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("a machines key must list machines: %s", toolText(res))
	}
	res, err = cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "list_services", Arguments: map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Fatal("a machines key reached a deploy-scoped tool")
	}
	if !regexp.MustCompile(`"code":\s*"scope_required"`).MatchString(toolText(res)) {
		t.Fatalf("the refusal is not hostd's own body: %s", toolText(res))
	}
}

func TestLoopbackURL(t *testing.T) {
	for in, want := range map[string]string{
		":8080":           "http://127.0.0.1:8080",
		"0.0.0.0:8080":    "http://127.0.0.1:8080",
		"[::]:9000":       "http://127.0.0.1:9000",
		"10.0.0.5:8080":   "http://10.0.0.5:8080",
		"localhost:8080":  "http://localhost:8080",
		"[fd00::1]:8080":  "http://[fd00::1]:8080",
		"127.0.0.1:18080": "http://127.0.0.1:18080",
	} {
		if got := LoopbackURL(in); got != want {
			t.Errorf("LoopbackURL(%q) = %q, want %q", in, got, want)
		}
	}
}
