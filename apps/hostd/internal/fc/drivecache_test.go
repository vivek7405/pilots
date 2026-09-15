package fc

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Every drive this package configures sets a cache type.
//
// # The bug this exists for
//
// Firecracker's default cache type is Unsafe, and Unsafe does not advertise
// the VirtIO flush feature at all. A guest that calls fsync on such a drive
// gets success back with the data sitting in the HOST's page cache, reaching
// the backing file only on writeback tens of seconds later.
//
// The Drive struct says so, and CacheTypeWriteback says so at length, and the
// volume drive follows it. The ROOTFS drive did not, so the durability the
// product claims was true of volumes and false of the disk every machine
// actually runs on. A guest could write a file, call sync, panic two seconds
// later, and the write was never there.
//
// Measured on the rig before the fix: 25 MiB written and synced in the guest
// left the copy-on-write file at zero bytes; a flush on the host delivered all
// 26 MiB at once.
//
// # Why this is structural
//
// Because the rule was already written down in two places and a drive was
// added without it anyway. A comment cannot fail. This can, with the name of
// whoever adds the third drive.
func TestEveryDriveSetsACacheType(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var offenders []string
	seen := 0

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
			lit, ok := n.(*ast.CompositeLit)
			if !ok {
				return true
			}
			ident, ok := lit.Type.(*ast.Ident)
			if !ok || ident.Name != "Drive" {
				return true
			}
			seen++
			for _, elt := range lit.Elts {
				kv, ok := elt.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				if key, ok := kv.Key.(*ast.Ident); ok && key.Name == "CacheType" {
					return true
				}
			}
			offenders = append(offenders, fset.Position(lit.Pos()).String())
			return true
		})
	}

	// A matcher that finds nothing is a test that guards nothing, and both
	// drives live in this package.
	if seen < 2 {
		t.Fatalf("only %d Drive literals found; the matcher has stopped seeing "+
			"them and this test is green for the wrong reason", seen)
	}
	for _, o := range offenders {
		t.Errorf("a Drive is configured with no CacheType at %s.\n"+
			"Firecracker defaults to Unsafe, which does not advertise VirtIO "+
			"flush: the guest's fsync returns success and the data sits in the "+
			"host's page cache. Set CacheTypeWriteback, or say in a comment why "+
			"this drive holds nothing anyone expects to survive.", o)
	}
}
