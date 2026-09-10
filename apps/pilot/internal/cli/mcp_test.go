package cli

import (
	"context"
	"slices"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	pilotsmcp "github.com/vivek7405/pilots/agents/mcp"
	pilots "github.com/vivek7405/pilots/sdks/go"

	"github.com/vivek7405/pilots/cli/internal/out"
)

// `pilot mcp` is the fleet toolset plus the local tools, and nothing else:
// the e2e battery asserts the same 26 names against the running binary, and
// this is the half of that check that needs no KVM.
func TestStdioServerIsFleetPlusLocal(t *testing.T) {
	d := mcpDeps{
		client: pilots.New("k", pilots.WithBaseURL("http://127.0.0.1:1")),
		getenv: func(string) string { return "" },
		env:    &Env{W: out.New(false)},
	}
	s := buildMCPServer(d)
	ct, st := mcp.NewInMemoryTransports()
	ctx := context.Background()
	if _, err := s.Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "t", Version: "0"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	res, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := make([]string, 0, len(res.Tools))
	for _, tool := range res.Tools {
		got = append(got, tool.Name)
	}
	slices.Sort(got)
	want := slices.Concat(pilotsmcp.FleetTools, pilotsmcp.LocalTools)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("stdio tools drifted:\n got %v\nwant %v", got, want)
	}
}
