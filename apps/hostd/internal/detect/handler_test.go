package detect

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vivek7405/pilots/hostd/internal/compose"
)

func TestTheHandlerPlansATarredRecipe(t *testing.T) {
	rec := post(t, tarOf(t, filepath.Join(fixtures, "webjs")), "?app=fx")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var got compose.PlanResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decoding: %v (%s)", err, rec.Body.String())
	}
	if got.Plan.App != "fx" {
		t.Errorf("app = %q, want fx: ?app= wins over package.json", got.Plan.App)
	}
	if len(got.Detected) != 1 || got.Detected[0].Source != "recipe" ||
		got.Detected[0].Framework != "webjs" {
		t.Fatalf("detected = %+v, want one webjs recipe", got.Detected)
	}
	if got.Detected[0].Health == nil || got.Detected[0].Health.Path != "/__webjs/ready" {
		t.Errorf("health = %+v, want the readiness path", got.Detected[0].Health)
	}
	if !strings.Contains(got.Plan.Steps[0].Dockerfile, "ENV PORT=8080") {
		t.Error("the step carries no generated Dockerfile")
	}
}

// The refusal has to be usable: an agent writes the Dockerfile from details
// alone, so the two rules and the listing travel with it.
func TestTheHandlerRefusesAnUnknownDirectoryWithEverythingNeededToFixIt(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "README.md", "# nothing to see")
	rec := post(t, tarOf(t, dir), "")

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	var got struct {
		Code    string                 `json:"code"`
		Next    string                 `json:"next"`
		Details compose.UnknownDetails `json:"details"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decoding: %v (%s)", err, rec.Body.String())
	}
	if got.Code != "unknown_framework" {
		t.Errorf("code = %q, want unknown_framework", got.Code)
	}
	if got.Next == "" {
		t.Error("the refusal says nothing about what to do next")
	}
	if len(got.Details.Rules) != 2 {
		t.Errorf("rules = %v, want the two Dockerfile rules", got.Details.Rules)
	}
	if len(got.Details.LookedFor) != 10 {
		t.Errorf("looked_for has %d entries, want 10", len(got.Details.LookedFor))
	}
}

// The tar is untrusted. `../escape` inside one is the oldest trick there is,
// and the extractor refuses it rather than clamping it back inside the root.
func TestTheHandlerRefusesAnEscapingPath(t *testing.T) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	body := []byte("owned")
	if err := tw.WriteHeader(&tar.Header{
		Name: "../escape", Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}

	rec := post(t, bytes.NewReader(buf.Bytes()), "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"code":"bad_request"`) {
		t.Errorf("body = %s, want a bad_request code", rec.Body.String())
	}
}

// ---------------------------------------------------------------------------

func post(t *testing.T, body io.Reader, query string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/plan"+query, body)
	req.Header.Set("Content-Type", "application/x-tar")
	rec := httptest.NewRecorder()
	Handler(t.TempDir())(rec, req)
	return rec
}

func tarOf(t *testing.T, dir string) io.Reader {
	t.Helper()
	r, err := TarDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// TarDir and ExtractContext are each other's inverse, which is what the push
// path depends on: it unpacks a GitHub tarball, writes a Dockerfile into it,
// and repacks it for the builder.
func TestTarDirRoundTrips(t *testing.T) {
	src := t.TempDir()
	write(t, src, "a.txt", "one")
	mkdir(t, src, "sub")
	write(t, filepath.Join(src, "sub"), "b.txt", "two")

	dst := t.TempDir()
	r, err := TarDir(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := extractInto(r, dst); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{"a.txt": "one", "sub/b.txt": "two"} {
		got, err := os.ReadFile(filepath.Join(dst, name))
		if err != nil {
			t.Fatalf("reading %s: %v", name, err)
		}
		if string(got) != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
}

func extractInto(r io.Reader, dir string) error {
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		path := filepath.Join(dir, hdr.Name)
		if hdr.Typeflag == tar.TypeDir {
			if err := os.MkdirAll(path, 0o755); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		f, err := os.Create(path)
		if err != nil {
			return err
		}
		if _, err := io.Copy(f, tr); err != nil {
			f.Close()
			return err
		}
		f.Close()
	}
}

// The context is unpacked under the root it was given, not under the process
// temp dir.
//
// os.MkdirTemp("") resolves to /tmp, which on a systemd host is very commonly
// tmpfs, so the difference is whether an authenticated caller can extract
// 2 GiB into the RAM of a host that is also running other tenants' microVMs.
// Asserted by handing the handler a root and watching a staging directory
// appear inside it, because nothing about the answer says where it staged.
func TestTheHandlerStagesUnderTheRootItWasGiven(t *testing.T) {
	root := filepath.Join(t.TempDir(), "plan-work")

	staged := make(chan []string, 1)
	done := make(chan struct{})
	go func() {
		// Sampled while the handler runs: the directory is removed on return,
		// so a look afterwards finds nothing either way.
		for {
			select {
			case <-done:
				return
			default:
			}
			if entries, err := os.ReadDir(root); err == nil && len(entries) > 0 {
				names := make([]string, 0, len(entries))
				for _, e := range entries {
					names = append(names, e.Name())
				}
				select {
				case staged <- names:
				default:
				}
				return
			}
		}
	}()

	req := httptest.NewRequest(http.MethodPost, "/v1/plan", tarOf(t, filepath.Join(fixtures, "webjs")))
	rec := httptest.NewRecorder()
	Handler(root)(rec, req)
	close(done)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	select {
	case names := <-staged:
		if len(names) != 1 || !strings.HasPrefix(names[0], "pilot-plan-") {
			t.Errorf("staged %v under the root, want one pilot-plan-* directory", names)
		}
	default:
		t.Fatal("nothing was staged under the root; the context went to the process temp dir")
	}

	// And it cleans up after itself: a plan route that left every context
	// behind would fill the cache root instead of tmpfs, which is not better.
	if entries, err := os.ReadDir(root); err != nil || len(entries) != 0 {
		t.Errorf("the root still holds %v after the handler returned", entries)
	}
}
