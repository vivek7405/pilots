package api

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/vivek7405/pilots/hostd/internal/state"
)

// internalDir is internal/, walked from this package's directory.
const internalDir = ".."

// TestEveryWriteErrorUsesAListedCode walks every WriteError call under
// internal/ and refuses a code that is not in the closed list.
//
// Without this the list is a suggestion. A code invented at one call site
// costs nothing to write and breaks every client that branches on codes,
// because the SDKs cannot know about it and the docs page cannot list it, and
// nothing anywhere else would be red.
func TestEveryWriteErrorUsesAListedCode(t *testing.T) {
	known := map[string]bool{}
	for _, c := range Codes {
		known[c] = true
	}
	// The constant names, so a call site spelling CodeFoo is checked too.
	constOf := map[string]string{
		"CodeBadRequest": CodeBadRequest, "CodeUnauthorized": CodeUnauthorized,
		"CodeScopeRequired": CodeScopeRequired, "CodeNotFound": CodeNotFound,
		"CodeConflict": CodeConflict, "CodeVolumeInUse": CodeVolumeInUse,
		"CodeQuotaExceeded": CodeQuotaExceeded, "CodeNotConfigured": CodeNotConfigured,
		"CodeNotImplemented": CodeNotImplemented, "CodeUnavailable": CodeUnavailable,
		"CodeInternal": CodeInternal, "CodePlanUnsupported": CodePlanUnsupported,
		"CodeComposeInvalid": CodeComposeInvalid, "CodeUnknownFramework": CodeUnknownFramework,
		"CodePlanMultiService": CodePlanMultiService, "CodeBuildFailed": CodeBuildFailed,
		"CodeHealthGateFailed": CodeHealthGateFailed, "CodeRepoNotConnected": CodeRepoNotConnected,
		"CodePayloadTooLarge": CodePayloadTooLarge, "CodeNoCapacity": CodeNoCapacity,
	}

	calls := 0
	err := filepath.Walk(internalDir, func(path string, info fs.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") ||
			strings.HasSuffix(path, "_test.go") {
			return err
		}
		fset := token.NewFileSet()
		f, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			return perr
		}
		ast.Inspect(f, func(n ast.Node) bool {
			// An ErrorResponse built as a literal rather than written through
			// WriteError -- mapError does this, so that one mapping can feed
			// both a status and a build-log line. Without this branch those
			// codes left the closed list the moment they stopped being
			// WriteError arguments.
			if lit, ok := n.(*ast.CompositeLit); ok && isErrorResponse(lit.Type) {
				name, isName := errorResponseCode(lit)
				if isName && !known[constOf[name]] {
					t.Errorf("%s: ErrorResponse carries the code %s, which is not in Codes",
						fset.Position(lit.Pos()), name)
				}
				return true
			}
			call, ok := n.(*ast.CallExpr)
			if !ok || !isWriteError(call.Fun) || len(call.Args) < 3 {
				return true
			}
			calls++
			name, ok := codeArg(call.Args[2])
			if !ok {
				t.Errorf("%s: WriteError's code is not a constant or a literal",
					fset.Position(call.Pos()))
				return true
			}
			if v, isConst := constOf[name]; isConst {
				if !known[v] {
					t.Errorf("%s: %s is not in Codes", fset.Position(call.Pos()), name)
				}
				return true
			}
			t.Errorf("%s: WriteError was given the literal %q; use a Code constant",
				fset.Position(call.Pos()), name)
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", internalDir, err)
	}
	// A walk that found nothing would pass vacuously, which is the one way
	// this test can rot without anyone noticing.
	if calls < 30 {
		t.Fatalf("only %d WriteError calls found under internal/; the sweep moved?", calls)
	}
}

// isErrorResponse reports whether a composite literal is an ErrorResponse,
// spelled bare or qualified.
func isErrorResponse(t ast.Expr) bool {
	switch e := t.(type) {
	case *ast.Ident:
		return e.Name == "ErrorResponse"
	case *ast.SelectorExpr:
		return e.Sel.Name == "ErrorResponse"
	}
	return false
}

// errorResponseCode reads the Code field of an ErrorResponse literal, and
// whether it names something this test can check. A bare identifier that is
// not a Code<Name> is a parameter being passed through -- WriteError's own
// body is the one that does that, and its call sites are checked above.
func errorResponseCode(lit *ast.CompositeLit) (string, bool) {
	for _, el := range lit.Elts {
		kv, ok := el.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		key, ok := kv.Key.(*ast.Ident)
		if !ok || key.Name != "Code" {
			continue
		}
		name, ok := codeArg(kv.Value)
		if !ok {
			return "", false
		}
		// A string literal is always checked, and always fails: constOf has
		// no entry for it, so it reports as not in Codes, which is what a
		// hand-spelled code should do.
		if _, isLit := kv.Value.(*ast.BasicLit); isLit {
			return name, true
		}
		if !strings.HasPrefix(name, "Code") {
			return "", false
		}
		return name, true
	}
	return "", false
}

func isWriteError(fun ast.Expr) bool {
	switch f := fun.(type) {
	case *ast.Ident:
		return f.Name == "WriteError"
	case *ast.SelectorExpr:
		return f.Sel.Name == "WriteError"
	}
	return false
}

// codeArg reads the third argument's name: a bare Code<Name>, api.Code<Name>,
// or a string literal (which the caller then reports as an error).
func codeArg(a ast.Expr) (string, bool) {
	switch e := a.(type) {
	case *ast.Ident:
		return e.Name, true
	case *ast.SelectorExpr:
		return e.Sel.Name, true
	case *ast.BasicLit:
		return strings.Trim(e.Value, `"`), true
	}
	return "", false
}

// TestWriteMappedLeaksNoInternals proves the two things a body must never
// carry: the store's own sentinel text, and a host-internal address.
//
// Both were in the answer before this contract landed: state.ErrNotFound's
// "state: not found" reached clients verbatim, and the health gate printed the
// probe URL, a 10.x address inside a netns that no caller can reach.
func TestWriteMappedLeaksNoInternals(t *testing.T) {
	tenDot := regexp.MustCompile(`\b10\.\d+\.\d+\.\d+\b`)

	cases := []struct {
		name   string
		err    error
		status int
		code   string
	}{
		{"not found", errNotFoundFixture(), 404, CodeNotFound},
		// The branch that used to answer with the sentinel verbatim: its text
		// is "state: this host does not own that machine".
		{"not owner", fmt.Errorf("put service svc_1: %w", state.ErrNotOwner), 409, CodeConflict},
		{"health gate", &HealthGateDetails{
			Service: "svc_1", Replica: "m_9", Release: "rel_2", GraceSec: 40,
			Last: HealthLast{Error: "connection refused on port 8080: the app is not listening on 0.0.0.0:$PORT"},
		}, 422, CodeHealthGateFailed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			writeMapped(rec, tc.err)
			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d", rec.Code, tc.status)
			}
			body := rec.Body.String()
			if !strings.Contains(body, `"code":"`+tc.code+`"`) {
				t.Fatalf("body has no %s code: %s", tc.code, body)
			}
			if !strings.Contains(body, `"next":"`) {
				t.Fatalf("a 4xx with no next: %s", body)
			}
			if strings.Contains(body, "state:") {
				t.Fatalf("the store's sentinel text reached the body: %s", body)
			}
			if tenDot.MatchString(body) {
				t.Fatalf("a host-internal address reached the body: %s", body)
			}
		})
	}
}

// TestHealthGateDetailsErrorReadsWithoutAnAddress checks the sentence the 422
// carries, both shapes of last answer.
func TestHealthGateDetailsErrorReadsWithoutAnAddress(t *testing.T) {
	g := &HealthGateDetails{Service: "web", Replica: "m_1", GraceSec: 40,
		Last: HealthLast{Error: "connection refused on port 8080"}}
	if got, want := g.Error(),
		"replica m_1 of web did not become healthy within 40s: connection refused on port 8080"; got != want {
		t.Fatalf("Error() = %q, want %q", got, want)
	}
	g = &HealthGateDetails{Replica: "m_2", GraceSec: 10,
		Last: HealthLast{Status: 503, Body: "starting"}}
	if got, want := g.Error(),
		"replica m_2 did not become healthy within 10s: last answer was 503 starting"; got != want {
		t.Fatalf("Error() = %q, want %q", got, want)
	}
}

// errNotFoundFixture is state.ErrNotFound, wrapped the way a store call
// returns it, so the leak test runs against the real sentinel text.
func errNotFoundFixture() error {
	return fmt.Errorf("getting machine m_1: %w", state.ErrNotFound)
}
