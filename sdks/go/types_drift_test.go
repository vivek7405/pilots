package pilots

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// The drift test.
//
// hostd's JSON tags are the contract. This SDK keeps a second copy of them,
// which is exactly the situation where a copy rots silently: a field added to
// Machine in Go would never reach a consumer and nothing would be red. So the
// copy is checked against its source on every go test.

const (
	apiDir     = "../../apps/hostd/internal/api"
	composeDir = "../../apps/hostd/internal/compose"
	// composePrefix is why compose.Step is ComposeStep here: Plan, Step and
	// Request are too generic to export unqualified.
	composePrefix = "Compose"
)

// goStructs returns each tagged struct in a directory and its JSON tag names.
func goStructs(t *testing.T, dir, prefix string) map[string][]string {
	t.Helper()
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, dir, func(fi fs.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("parsing %s: %v", dir, err)
	}

	out := map[string][]string{}
	for _, pkg := range pkgs {
		for _, file := range pkg.Files {
			ast.Inspect(file, func(n ast.Node) bool {
				spec, ok := n.(*ast.TypeSpec)
				if !ok {
					return true
				}
				st, ok := spec.Type.(*ast.StructType)
				if !ok {
					return true
				}
				var tags []string
				for _, field := range st.Fields.List {
					if field.Tag == nil {
						continue
					}
					raw, err := strconv.Unquote(field.Tag.Value)
					if err != nil {
						continue
					}
					name, _, _ := strings.Cut(reflect.StructTag(raw).Get("json"), ",")
					if name != "" && name != "-" {
						tags = append(tags, name)
					}
				}
				// A struct with no tags is not a wire shape (api.Deps, say).
				if len(tags) > 0 {
					out[prefix+spec.Name.Name] = tags
				}
				return true
			})
		}
	}
	return out
}

// hostdWireStructs is every wire shape hostd serves, across both packages.
func hostdWireStructs(t *testing.T) map[string][]string {
	t.Helper()
	all := goStructs(t, apiDir, "")
	// internal/compose arrives with #30. Until then there is nothing to walk;
	// the day it lands its structs are checked without touching this file.
	if _, err := os.Stat(composeDir); err == nil {
		for name, tags := range goStructs(t, composeDir, composePrefix) {
			all[name] = tags
		}
	}
	return all
}

// mirrored is every struct listed in wireTypes, by name, with its tag names.
func mirrored(t *testing.T) map[string][]string {
	t.Helper()
	out := map[string][]string{}
	for _, v := range wireTypes {
		rt := reflect.TypeOf(v)
		if rt.Kind() != reflect.Struct {
			t.Fatalf("wireTypes carries a non-struct %v", rt)
		}
		var tags []string
		for i := range rt.NumField() {
			name, _, _ := strings.Cut(rt.Field(i).Tag.Get("json"), ",")
			if name != "" && name != "-" {
				tags = append(tags, name)
			}
		}
		if _, dup := out[rt.Name()]; dup {
			t.Fatalf("wireTypes lists %s twice", rt.Name())
		}
		out[rt.Name()] = tags
	}
	return out
}

func TestParserFoundSomethingToCompare(t *testing.T) {
	// A moved or renamed directory has to fail loudly rather than pass
	// vacuously by finding nothing on either side.
	got := goStructs(t, apiDir, "")
	if len(got) < 20 {
		abs, _ := filepath.Abs(apiDir)
		t.Fatalf("only %d tagged structs found under %s; did the directory move?", len(got), abs)
	}
	if len(wireTypes) < 20 {
		t.Fatalf("only %d entries in wireTypes", len(wireTypes))
	}
}

func TestTypesMirrorHostd(t *testing.T) {
	hostd := hostdWireStructs(t)
	mine := mirrored(t)

	for name, want := range hostd {
		got, ok := mine[name]
		if !ok {
			t.Errorf("sdks/go/types.go: no struct %s, and no wireTypes entry for it (hostd has %v)",
				name, want)
			continue
		}
		if missing := diff(want, got); len(missing) > 0 {
			t.Errorf("sdks/go/types.go: %s is missing json tags %v (hostd %s)", name, missing, name)
		}
		if extra := diff(got, want); len(extra) > 0 {
			t.Errorf("sdks/go/types.go: %s carries tags hostd no longer has: %v", name, extra)
		}
	}
}

func TestFrameConstantsMatchHostd(t *testing.T) {
	src, err := os.ReadFile(filepath.Join(apiDir, "types.go"))
	if err != nil {
		t.Fatalf("reading hostd types.go: %v", err)
	}
	want := map[string]byte{}
	for line := range strings.Lines(string(src)) {
		fields := strings.Fields(line)
		// FrameStdout byte = 1
		if len(fields) != 4 || !strings.HasPrefix(fields[0], "Frame") || fields[1] != "byte" || fields[2] != "=" {
			continue
		}
		n, err := strconv.Atoi(fields[3])
		if err != nil {
			continue
		}
		want[fields[0]] = byte(n)
	}
	if len(want) < 3 {
		t.Fatalf("no Frame constants found in hostd types.go")
	}

	mine := map[string]byte{
		"FrameStdin": FrameStdin, "FrameStdout": FrameStdout, "FrameStderr": FrameStderr,
		"FrameExit": FrameExit, "FrameStdinEOF": FrameStdinEOF,
	}
	for name, value := range want {
		got, ok := mine[name]
		if !ok {
			t.Errorf("sdks/go/types.go does not declare %s (hostd has it = %d)", name, value)
			continue
		}
		if got != value {
			t.Errorf("sdks/go/types.go %s is %d, hostd says %d", name, got, value)
		}
	}
}

// diff returns everything in a that is not in b.
func diff(a, b []string) []string {
	var out []string
	for _, v := range a {
		if !slices.Contains(b, v) {
			out = append(out, v)
		}
	}
	return out
}
