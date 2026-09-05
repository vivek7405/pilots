package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// Only the MACHINE ownership question is filtered by CPU vendor. Every other
// caller of the ranking must keep asking it over the whole live set.
//
// This is a single-writer invariant and it fails SILENTLY. A service, a domain
// and a GitHub repo delivery each name one arbiter and have no host_id column
// to guard the write on -- the deterministic ranking IS the guard. Split that
// candidate set by vendor on a mixed fleet and an Intel host and an AMD host
// compute different arbiters for the same service, both write it, and cr-sqlite
// merges the two per column: no error, no conflict, half of each write kept.
// Nothing in the system would ever report it.
//
// Structural because the mistake is a one-word edit at a call site, and because
// every behavioural path that could show it needs a real two-vendor fleet.
func TestOnlyTheMachineRankingIsVendorFiltered(t *testing.T) {
	callers := callersAcrossHostd(t)

	// MachineOwnerFor narrows. RescuerFor is the one function allowed to call
	// it, so that the router's held-request rescue and the self-heal loop
	// cannot drift apart -- there is one definition of who rescues a machine.
	if got := callers["MachineOwnerFor"]; strings.Join(got, ",") != "RescuerFor" {
		t.Errorf("MachineOwnerFor is called from %v, want exactly [RescuerFor]: "+
			"a second caller is a second definition of who brings a machine back, "+
			"and two hosts that disagree either both start it or neither does", got)
	}

	// OwnerFor does NOT narrow, and these are every place that asks it.
	want := []string{
		"NewOwnedID",          // internal/state: mints service ids this host arbitrates
		"MachineOwnerFor",     // internal/state: the narrowed form delegates here
		"MachineOwnerFor",     // ...twice: the pool first, then the whole fleet
		"assertServiceWriter", // internal/state/corrosion: the service row guard
		"mine",                // internal/api/forward.go: forward to the arbiter
		"serviceArbiter",      // internal/github: one host acts on a delivery
		"runDomainVerifier",   // cmd/hostd: the domain row writer
		"scaleOnce",           // internal/services: the autoscaler's arbiter
	}
	sort.Strings(want)
	if got := callers["OwnerFor"]; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("OwnerFor is called from\n  %v\nwant\n  %v\n"+
			"A caller that moved to MachineOwnerFor has had its arbiter partitioned by "+
			"CPU vendor, which gives one row two writers and corrupts it through the "+
			"merge with nothing to notice. A NEW caller belongs in this list only after "+
			"someone has decided which of the two it is.", got, want)
	}
}

// callersAcrossHostd maps a function name to the names of the functions in the
// whole hostd module that call it, sorted, duplicates kept.
func callersAcrossHostd(t *testing.T) map[string][]string {
	t.Helper()

	root, err := filepath.Abs("../..") // the hostd module root
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	out := map[string][]string{}

	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		parsed, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			return perr
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
				name := ""
				switch fun := call.Fun.(type) {
				case *ast.SelectorExpr:
					name = fun.Sel.Name
				case *ast.Ident:
					name = fun.Name
				}
				if name == "OwnerFor" || name == "MachineOwnerFor" {
					out[name] = append(out[name], fn.Name.Name)
				}
				return true
			})
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for k := range out {
		sort.Strings(out[k])
	}
	return out
}
