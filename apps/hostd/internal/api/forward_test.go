package api

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"testing"
)

// The forwarding marker has ONE name across the fleet.
//
// It had two. This package marked an arbiter forward, and cmd/hostd marked
// every host-to-peer call, "X-Pilots-Forwarded-By" -- while the internal
// listener those requests land on requires router.ForwardedHeader and answers
// 400 without it. So a cross-host service write, a cross-host wake or suspend,
// and the volume rollout's remote redeploy were all refused before a handler
// saw them, and the public listener's strip -- which is the whole reason
// WithAuth may treat the marker as proof a request came over the mesh -- was
// deleting a header nobody set.
//
// Read out of the router's source rather than copied here, because a copy is
// exactly what this test exists to prevent. api cannot import router: router
// imports machines, which imports api.
func TestTheForwardingMarkerHasOneName(t *testing.T) {
	const src = "../router/crosshost.go"
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, src, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", src, err)
	}

	var want string
	ast.Inspect(f, func(n ast.Node) bool {
		spec, ok := n.(*ast.ValueSpec)
		if !ok || len(spec.Names) != 1 || spec.Names[0].Name != "ForwardedHeader" {
			return true
		}
		lit, ok := spec.Values[0].(*ast.BasicLit)
		if ok && lit.Kind == token.STRING {
			want, _ = strconv.Unquote(lit.Value)
		}
		return false
	})
	if want == "" {
		t.Fatalf("could not find router.ForwardedHeader in %s", src)
	}
	if forwardedHeader != want {
		t.Errorf("this package marks a forwarded request %q and the internal listener "+
			"that receives it requires %q; every forwarded call is a 400 before it "+
			"reaches a handler", forwardedHeader, want)
	}
}
