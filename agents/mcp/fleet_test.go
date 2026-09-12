package pilotsmcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/vivek7405/pilots/agents"
	pilots "github.com/vivek7405/pilots/sdks/go"
)

// fakeAPI answers the two routes the tests exercise, and records the bearer
// key it was called with.
func fakeAPI(t *testing.T) (*httptest.Server, *string) {
	t.Helper()
	var seen string
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/machines", func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"id":"m-1","name":"demo","state":"running","url":"https://demo.test","labels":{"team":"a"}},` +
			`{"id":"m-2","name":"other","state":"suspended","labels":{}}]`))
	})
	mux.HandleFunc("GET /v1/machines/{id}", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("id") != "m-1" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"no such machine","code":"not_found","next":"pilot machines ls"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"m-1","name":"demo","state":"running","url":"https://demo.test"}`))
	})
	mux.HandleFunc("GET /v1/services", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":"this key cannot deploy","code":"scope_required","next":"mint a deploy key","details":{"need":"deploy"}}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, &seen
}

func connect(t *testing.T, s *mcp.Server) *mcp.ClientSession {
	t.Helper()
	ct, st := mcp.NewInMemoryTransports()
	ctx := context.Background()
	if _, err := s.Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	c := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil)
	cs, err := c.Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

func text(r *mcp.CallToolResult) string {
	var b strings.Builder
	for _, c := range r.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}

func TestFleetToolsAreExactlyTheList(t *testing.T) {
	api, _ := fakeAPI(t)
	cs := connect(t, NewServer(pilots.New("k", pilots.WithBaseURL(api.URL)), Options{}))
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(res.Tools))
	for _, tool := range res.Tools {
		names = append(names, tool.Name)
		if len(tool.Description) < 40 {
			t.Errorf("%s has no useful description", tool.Name)
		}
	}
	slices.Sort(names)
	if !slices.Equal(names, FleetTools) {
		t.Fatalf("tools drifted:\n got %v\nwant %v", names, FleetTools)
	}
	if !slices.IsSorted(FleetTools) || !slices.IsSorted(LocalTools) {
		t.Fatal("the tool lists must stay sorted; the e2e battery compares them sorted")
	}
	for _, name := range LocalTools {
		if slices.Contains(FleetTools, name) {
			t.Errorf("%s is in both lists", name)
		}
	}
	if len(FleetTools)+len(LocalTools) != 30 {
		t.Fatalf("the two lists hold %d tools; the battery expects 30", len(FleetTools)+len(LocalTools))
	}
}

func TestToolsCarryTheCallersKeyAndTheNextStep(t *testing.T) {
	api, seen := fakeAPI(t)
	cs := connect(t, NewServer(pilots.New("secret-key", pilots.WithBaseURL(api.URL)), Options{}))
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "list_machines", Arguments: map[string]any{"labels": map[string]string{"team": "a"}}})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("list_machines failed: %s", text(res))
	}
	if *seen != "Bearer secret-key" {
		t.Fatalf("the API saw %q", *seen)
	}
	var body struct {
		Result []pilots.Machine `json:"result"`
		Next   string           `json:"next"`
	}
	if err := json.Unmarshal([]byte(text(res)), &body); err != nil {
		t.Fatalf("%v in %s", err, text(res))
	}
	if len(body.Result) != 1 || body.Result[0].ID != "m-1" {
		t.Fatalf("the label filter did not apply: %s", text(res))
	}

	// A name resolves to a machine.
	res, err = cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "status", Arguments: map[string]any{"machine": "demo"}})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError || !strings.Contains(text(res), `"id": "m-1"`) {
		t.Fatalf("status by name: %s", text(res))
	}
}

// An API refusal reaches the agent as hostd's own body, code and next intact,
// rather than a Go error string.
func TestRefusalIsTheServersOwnBody(t *testing.T) {
	api, _ := fakeAPI(t)
	cs := connect(t, NewServer(pilots.New("k", pilots.WithBaseURL(api.URL)), Options{}))
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "list_services", Arguments: map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Fatal("a 403 must be an error result")
	}
	if !strings.Contains(text(res), `"code":"scope_required"`) || !strings.Contains(text(res), `"next":"mint a deploy key"`) {
		t.Fatalf("the body lost its code or next: %s", text(res))
	}
}

func TestInitAndDocs(t *testing.T) {
	api, _ := fakeAPI(t)
	hosted := connect(t, NewServer(pilots.New("k", pilots.WithBaseURL(api.URL)), Options{}))
	res, err := hosted.CallTool(context.Background(), &mcp.CallToolParams{Name: "init", Arguments: map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	var body struct {
		Primer string   `json:"primer"`
		Topics []string `json:"topics"`
		Local  string   `json:"local_tools"`
	}
	if err := json.Unmarshal([]byte(text(res)), &body); err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(body.Primer, "\n"); n >= 60 {
		t.Errorf("the primer is %d lines; it must stay under sixty", n)
	}
	if !strings.Contains(strings.Join(strings.SplitN(body.Primer, "\n", 10)[:10], "\n"), "deploy") {
		t.Error("the primer does not name deploy in its first ten lines")
	}
	if !slices.Equal(body.Topics, agents.Topics()) {
		t.Errorf("topics %v != embedded %v", body.Topics, agents.Topics())
	}
	if !strings.Contains(body.Local, "pilot mcp") {
		t.Errorf("the hosted init does not say where the local tools are: %q", body.Local)
	}

	local := connect(t, NewServer(pilots.New("k", pilots.WithBaseURL(api.URL)), Options{Local: true}))
	res, err = local.CallTool(context.Background(), &mcp.CallToolParams{Name: "init", Arguments: map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(text(res), "local_tools") {
		t.Error("a server with the local tools must not claim to lack them")
	}

	res, err = hosted.CallTool(context.Background(), &mcp.CallToolParams{Name: "docs", Arguments: map[string]any{"topic": "deploy"}})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError || !strings.Contains(text(res), `"topic": "deploy"`) {
		t.Fatalf("docs deploy: %s", text(res))
	}
	res, err = hosted.CallTool(context.Background(), &mcp.CallToolParams{Name: "docs", Arguments: map[string]any{"query": "0.0.0.0"}})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError || !strings.Contains(text(res), `"excerpt"`) {
		t.Fatalf("docs search: %s", text(res))
	}
	res, err = hosted.CallTool(context.Background(), &mcp.CallToolParams{Name: "docs", Arguments: map[string]any{"topic": "nope"}})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || !strings.Contains(text(res), "the topics are") {
		t.Fatalf("an unknown topic must list the topics: %s", text(res))
	}
}

func TestSkillIsServedAsResources(t *testing.T) {
	api, _ := fakeAPI(t)
	cs := connect(t, NewServer(pilots.New("k", pilots.WithBaseURL(api.URL)), Options{}))
	res, err := cs.ListResources(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	uris := map[string]bool{}
	for _, r := range res.Resources {
		uris[r.URI] = true
	}
	if !uris["pilots-docs://SKILL.md"] || !uris["pilots-docs://references/deploy.md"] {
		t.Fatalf("resources: %v", uris)
	}
	read, err := cs.ReadResource(context.Background(), &mcp.ReadResourceParams{URI: "pilots-docs://SKILL.md"})
	if err != nil {
		t.Fatal(err)
	}
	if len(read.Contents) != 1 || !strings.HasPrefix(read.Contents[0].Text, "---\nname: pilots") {
		t.Fatalf("SKILL.md read back wrong: %+v", read.Contents)
	}
}
