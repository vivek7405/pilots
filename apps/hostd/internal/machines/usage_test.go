package machines

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/vivek7405/pilots/hostd/internal/state"
)

// The meter is wired to the state writes, and to exactly those.
//
// There is no behavioural test for this here, and there cannot be one: every
// path that carries a hook -- Create, Suspend, Wake, Rescue -- needs a
// Firecracker host and a real snapshot before it will run a line. The e2e
// battery asserts the numbers end to end on such a host; this asserts the
// shape here, where it costs nothing and cannot be skipped.
//
// A missing hook does not fail anything at runtime. It silently bills a
// suspended machine for compute it is not using, or keeps billing a machine
// that has been destroyed -- money, quietly wrong, with every test green.
func TestEveryLifecycleWriteHasItsLedgerHook(t *testing.T) {
	got := ledgerHooks(t)
	want := map[string][]string{
		"Open":       {"Create", "Rescue"},
		"Transition": {"Suspend", "Wake", "Wake"},
		"Close":      {"Destroy", "StopLocal"},
	}
	for method, wantCallers := range want {
		if strings.Join(got[method], ",") != strings.Join(wantCallers, ",") {
			t.Errorf("m.opts.Usage.%s is called from %v, want exactly %v",
				method, got[method], wantCallers)
		}
	}
	// Wake carries two: running on the restore that worked, error on the one
	// that did not. A machine left in error still holds its row and its disk,
	// so wall time and storage keep accruing and compute stops.
	if len(got["Transition"]) != 3 {
		t.Errorf("Transition has %d call sites, want three", len(got["Transition"]))
	}
}

// ledgerHooks maps a Ledger method to the functions in this package that call
// it on m.opts.Usage, sorted, duplicates kept.
func ledgerHooks(t *testing.T) map[string][]string {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	out := map[string][]string{}
	for _, entry := range entries {
		file := entry.Name()
		if !strings.HasSuffix(file, ".go") || strings.HasSuffix(file, "_test.go") {
			continue
		}
		parsed, err := parser.ParseFile(fset, filepath.Join(".", file), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", file, err)
		}
		for _, decl := range parsed.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || !isUsageField(sel.X) {
					return true
				}
				out[sel.Sel.Name] = append(out[sel.Sel.Name], fn.Name.Name)
				return true
			})
		}
	}
	for k := range out {
		sort.Strings(out[k])
	}
	return out
}

// isUsageField reports whether an expression is m.opts.Usage.
func isUsageField(e ast.Expr) bool {
	outer, ok := e.(*ast.SelectorExpr)
	if !ok || outer.Sel.Name != "Usage" {
		return false
	}
	inner, ok := outer.X.(*ast.SelectorExpr)
	return ok && inner.Sel.Name == "opts"
}

func TestVolumeGiBIsTheRowsSizeAndZeroWithoutOne(t *testing.T) {
	st, err := state.Open(":memory:")
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	m := &Manager{opts: Options{HostID: "host-a", Store: st}, flight: newInFlight()}

	if err := st.PutVolume(context.Background(), &state.Volume{
		ID: "vol_1", Name: "pgdata", SizeMiB: 10 * 1024, HostID: "host-a",
	}); err != nil {
		t.Fatalf("PutVolume: %v", err)
	}

	if got := m.volumeGiB(context.Background(), "vol_1"); got != 10 {
		t.Errorf("volumeGiB = %d, want 10", got)
	}
	if got := m.volumeGiB(context.Background(), ""); got != 0 {
		t.Errorf("volumeGiB with no volume = %d, want 0", got)
	}
	// A read that fails must not fail the lifecycle operation that triggered
	// it: under-billing one interval's storage is the right way to be wrong.
	if got := m.volumeGiB(context.Background(), "vol_missing"); got != 0 {
		t.Errorf("volumeGiB for a missing volume = %d, want 0", got)
	}
}

// A manager built without a ledger is the fake and every test above. The hooks
// have no nil check at any call site, so this is what keeps that safe.
func TestAManagerWithNoLedgerIsSafeToMeter(t *testing.T) {
	m := &Manager{opts: Options{HostID: "host-a"}, flight: newInFlight()}
	m.opts.Usage.Open("m_1", "org_1", StateRunning, 1, 512, 0)
	m.opts.Usage.Transition("m_1", StateSuspended)
	m.opts.Usage.Close("m_1")
}
