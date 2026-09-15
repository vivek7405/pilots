package machines

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A restore is never asked for with no vmstate key.
//
// # The bug this exists for
//
// A restore needs three artifacts: the memory image, the disk, and the
// Firecracker vmstate holding device state and vcpu registers. The first two
// are build ids that travel on a release row and a checkpoint row. The third
// is an object key built from the machine and checkpoint it was captured from,
// and nothing carried it.
//
// So createFromRelease passed a literal "" and every replica restoring from a
// release fetched the empty key. The AWS SDK refused it before the request
// left the host, with "input member Key must not be empty" -- a message that
// names no release, no machine and no artifact. It reached the rig as a fork
// returning HTTP 500 with three joined errors, none of which was the one that
// mattered.
//
// # Why this is structural rather than behavioural
//
// Because every path that would catch it behaviourally -- a release rollout, a
// fork, a checkpoint restore -- needs a Firecracker host, a real template and
// a populated bucket before it executes one line. The e2e battery covers those
// on such a host. This covers the shape of the code, here, where it costs
// nothing and cannot be skipped, and it fails with the name of whoever adds
// the next empty key.
//
// A literal "" is the whole check. A variable that happens to be empty at
// runtime is a different failure, and createFromRelease refuses that one with
// a message that names the release.
func TestNoRestoreAsksForAnEmptyVMState(t *testing.T) {
	// The functions whose snapKey parameter must never be a literal "", and
	// which argument that is.
	guarded := map[string]int{
		"restoreInstant":          3,
		"restoreInstantImmutable": 3,
		"createFromRelease":       5,
		"startForRelease":         5,
	}

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var offenders []string

	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Join(".", name), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			idx, guardedCall := guarded[sel.Sel.Name]
			if !guardedCall || len(call.Args) <= idx {
				return true
			}
			lit, ok := call.Args[idx].(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			if lit.Value != `""` {
				return true
			}
			offenders = append(offenders,
				fset.Position(call.Pos()).String()+": "+sel.Sel.Name)
			return true
		})
	}

	for _, o := range offenders {
		t.Errorf("a restore is asked for with no vmstate key at %s.\n"+
			"A restore needs the memory image, the disk AND the vmstate. An empty "+
			"key is not 'no vmstate', it is a request the AWS SDK refuses with "+
			"\"input member Key must not be empty\", naming neither the machine nor "+
			"the artifact. Pass the key: checkpointSnapKey(machine, checkpoint) for "+
			"a checkpoint or a release, suspendSnapKey(machine) for a suspend image.", o)
	}
}

// The guard above is only worth having if it can see a call at all, so this
// proves the matcher fires. Without it a rename of restoreInstant would leave
// the test silently green and guarding nothing.
func TestTheEmptyVMStateGuardCanSeeARestoreCall(t *testing.T) {
	fset := token.NewFileSet()
	const src = `package p
func f(m *Manager) { m.restoreInstant(ctx, row, backends, "") }`
	file, err := parser.ParseFile(fset, "x.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "restoreInstant" || len(call.Args) <= 3 {
			return true
		}
		if lit, ok := call.Args[3].(*ast.BasicLit); ok && lit.Value == `""` {
			found = true
		}
		return true
	})
	if !found {
		t.Fatal("the matcher no longer recognises a restore called with an empty " +
			"vmstate key, so the guard above is green for the wrong reason")
	}
}
