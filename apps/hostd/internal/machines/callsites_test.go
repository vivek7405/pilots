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

// Every bring-up gets its netns index through takeSlot, which is the one place
// a kept reservation is consumed. A second pool.Take anywhere in this package
// is a path that burns a slot per wake -- #64 arriving again, with the name of
// whoever added it.
//
// captureTemplateMemory is the sanctioned exception: the throwaway machine a
// golden template is photographed from has no row, so it has no reservation to
// reuse, and it returns its index on the way out.
func TestEveryBringUpGetsItsSlotThroughTakeSlot(t *testing.T) {
	got := callersOf(t, "Take")
	want := []string{"captureTemplateMemory", "takeSlot"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("pool.Take is called from %v, want exactly %v: a bring-up that "+
			"takes its own index cannot consume the reservation a suspended "+
			"replica kept, so it leaks one slot per cycle", got, want)
	}
}

// Redeploy must not stamp the row to zero before it boots.
//
// The suspended replica it usually runs against still holds its index in the
// pool and on its row, and takeSlot reads the row: a stamp to zero ahead of
// the boot silently drops back to a fresh index, which looks fixed and leaks a
// slot per deploy. The interim row the STORE sees is zeroed by withoutSlot,
// which copies rather than mutates, so both facts stay true at once.
//
// Structural because Redeploy needs a Firecracker host and a real image before
// it runs a line, exactly as the env invariant above is.
func TestARedeployDoesNotClearTheSlotBeforeTheBoot(t *testing.T) {
	body := funcBody(t, "manager.go", "Redeploy")

	boot := -1
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if sel, ok := call.Fun.(*ast.SelectorExpr); ok &&
			sel.Sel.Name == "bootMachine" && boot < 0 {
			boot = int(call.Pos())
		}
		return true
	})
	if boot < 0 {
		t.Fatal("Redeploy no longer calls bootMachine; this test needs rewriting")
	}

	var hidden int
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		name := ""
		switch fn := call.Fun.(type) {
		case *ast.Ident:
			name = fn.Name
		case *ast.SelectorExpr:
			name = fn.Sel.Name
		}
		if name == "withoutSlot" && int(call.Pos()) < boot {
			hidden++
		}
		if name == "stampSlot" && int(call.Pos()) < boot {
			t.Error("Redeploy stamps the row's slot before the boot: the boot then " +
				"reads zero, takes a fresh index, and abandons the one a suspended " +
				"replica kept. Zero the copy the store gets, with withoutSlot.")
		}
		return true
	})
	if hidden != 1 {
		t.Errorf("Redeploy passes withoutSlot to %d calls before the boot, want 1: "+
			"the row the store sees while a machine has no process must name no "+
			"slot, or the mesh advertises an address nothing is listening on", hidden)
	}
}

// funcBody returns the body of the named top-level function or method in file.
func funcBody(t *testing.T, file, name string) *ast.BlockStmt {
	t.Helper()

	fset := token.NewFileSet()
	parsed, err := parser.ParseFile(fset, filepath.Join(".", file), nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", file, err)
	}
	for _, decl := range parsed.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name.Name == name {
			return fn.Body
		}
	}
	t.Fatalf("%s has no function named %s", file, name)
	return nil
}

// No path restores a memory image without first asking whose CPU vendor
// photographed it.
//
// A Firecracker snapshot carries raw CPUID and never restores across the
// Intel/AMD boundary. There is no flag on that decision, deliberately: it has
// exactly one home, bringUp, and Wake and Rescue reach it through no other
// door. A second caller of wakeFromSuspend is a path that will hand a foreign
// vmstate to Firecracker and write the machine StateError -- which looks like
// a broken machine rather than like the mistake it is.
//
// Structural for the reason the test above this one is: every path that could
// get it wrong needs a Firecracker host and a real snapshot before it runs a
// line, and this costs nothing and cannot be skipped.
func TestNoRestoreSkipsTheVendorCheck(t *testing.T) {
	for _, tc := range []struct {
		callee string
		want   []string
		why    string
	}{
		{
			callee: "wakeFromSuspend",
			want:   []string{"bringUp"},
			why: "a second caller restores a memory image without asking which " +
				"CPU vendor photographed it, and Firecracker refuses a foreign " +
				"vmstate at load rather than at a review",
		},
		{
			callee: "bringUp",
			want:   []string{"Rescue", "Wake"},
			why: "these are the two paths that bring a suspended machine back " +
				"locally; anything else reaching them is a bring-up outside the " +
				"lock and the ledger hooks both of these own",
		},
		{
			callee: "bootFromDisk",
			want:   []string{"bringUp", "restoreFromCheckpoint"},
			why: "the cold-boot path is entered from the vendor decision and " +
				"from a rollback whose CHECKPOINT is foreign, and from nowhere " +
				"else -- a caller that reached it directly would discard a " +
				"resumable memory image for no reason",
		},
		{
			callee: "restoreInstantImmutable",
			want:   []string{"restoreFromCheckpoint"},
			why: "a checkpoint restore is the only immutable-artifact restore, " +
				"and it is where the checkpoint's own vendor is checked",
		},
	} {
		got := callersOf(t, tc.callee)
		if strings.Join(got, ",") != strings.Join(tc.want, ",") {
			t.Errorf("%s is called from %v, want exactly %v: %s",
				tc.callee, got, tc.want, tc.why)
		}
	}
}
