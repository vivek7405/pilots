package machines

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// Delivery-and-exec happens on a create from template, and on nothing else.
//
// There is no behavioural test for this, and there cannot be one here: every
// path that could get it wrong -- Wake, Rescue, restoring a checkpoint -- needs
// a Firecracker host and a real snapshot before it will run a single line. The
// e2e battery covers it end to end on such a host, and this covers it here, on
// the shape of the code, where it costs nothing and cannot be skipped.
//
// The invariant is structural on purpose: deliverEnv is not guarded by a flag
// that a caller could set wrongly, it simply has ONE call site. Adding a
// second, anywhere in this package, fails here with the name of whoever added
// it -- which is the mistake the issue warns about, arriving as a test failure
// instead of as a machine that looks healthy and has lost its process.
func TestEnvIsDeliveredFromTheCreatePathAndNowhereElse(t *testing.T) {
	for _, tc := range []struct {
		callee string
		want   []string
		why    string
	}{
		{
			// Two create paths, not one. A machine with a volume or its own
			// image cannot be restored from the golden template -- a drive
			// cannot be added to a snapshot being resumed, and the template's
			// memory describes the template's disk -- so it boots instead.
			// Both are creates, and a create is the only moment an application
			// can be handed an environment it did not start with.
			callee: "deliverEnv",
			want:   []string{"bootMachine", "createFromTemplate"},
			why: "a wake resumes a snapshot in which the application is already " +
				"running, so delivering an environment there restarts the process " +
				"the guest just restored",
		},
		{
			callee: "createFromTemplate",
			want:   []string{"startNewMachine"},
			why: "reaching the create path from anywhere else would deliver an " +
				"environment on that path too, by the back door",
		},
		{
			callee: "bootMachine",
			want:   []string{"Redeploy", "startNewMachine"},
			why: "the same back door, by the other create path -- and from a " +
				"redeploy, which is a create of the process: the old one was " +
				"killed, the new one starts from another image and has to be " +
				"handed its environment exactly as a first boot is",
		},
		{
			// The dispatcher is the choke point both paths go through, so the
			// invariant survives as one assertion rather than two that could
			// drift apart: whatever reaches an environment reaches it through
			// here, and only Create reaches here.
			callee: "startNewMachine",
			want:   []string{"Create"},
			why: "both create paths run through this, so anything else calling " +
				"it would deliver an environment outside a create",
		},
	} {
		got := callersOf(t, tc.callee)
		if strings.Join(got, ",") != strings.Join(tc.want, ",") {
			t.Errorf("%s is called from %v, want exactly %v: %s",
				tc.callee, got, tc.want, tc.why)
		}
	}
}

// callersOf returns the names of the functions in this package that call name,
// sorted, with duplicates kept so that two calls from one function are visible.
func callersOf(t *testing.T, name string) []string {
	t.Helper()

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}

	fset := token.NewFileSet()
	var callers []string
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
			if !ok {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				// Method calls only: every one of these is m.<name>(...).
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if ok && sel.Sel.Name == name {
					callers = append(callers, fn.Name.Name)
				}
				return true
			})
		}
	}
	sort.Strings(callers)
	return callers
}
