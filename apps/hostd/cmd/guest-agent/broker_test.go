package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// A fetched token lands on disk, at 0600, and nowhere else.
//
// The mode is the assertion worth having: everything in a guest runs as root
// today, and the day something does not, this file should already be right
// rather than be found world-readable by whoever notices first.
func TestAFetchedTokenIsWrittenPrivately(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/token" {
			t.Errorf("asked for %s, want /token", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"token":"pbt1.abc.def","expires_at":1,"scopes":["machines"]}`))
	}))
	defer srv.Close()

	path := filepath.Join(t.TempDir(), "run", "pilot", "token")
	if err := fetchBrokerToken(context.Background(), srv.URL, path); err != nil {
		t.Fatalf("fetch: %v", err)
	}

	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(body) != "pbt1.abc.def" {
		t.Errorf("token file = %q", body)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600", info.Mode().Perm())
	}
}

// Deny is not an error. A machine with no grant is the ordinary case, and the
// loop has to stop rather than ask for ever: a retry loop against a decision
// somebody made is a request per interval, per machine, for the life of the
// fleet.
func TestNoGrantIsItsOwnAnswerAndStopsTheLoop(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"code":"no_grant"}`))
	}))
	defer srv.Close()

	path := filepath.Join(t.TempDir(), "token")
	err := fetchBrokerToken(context.Background(), srv.URL, path)
	if !errors.Is(err, errNoGrant) {
		t.Fatalf("err = %v, want errNoGrant so the loop stops", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("a refused fetch left a token file behind")
	}
}

// A withdrawn grant has to clear the file. Left alone, an application would
// keep sending a token that works until it expires, against a grant somebody
// deliberately removed.
func TestARefusalRemovesATokenThatWasAlreadyThere(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	if err := writeToken(path, "pbt1.old.token"); err != nil {
		t.Fatalf("writeToken: %v", err)
	}
	removeToken(path)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("the stale token survived")
	}
}

// A broker that answers rubbish must not leave a truncated token on disk, which
// a reader would send and get an unexplainable 401 for.
func TestAnEmptyOrUnreadableAnswerWritesNothing(t *testing.T) {
	for _, body := range []string{`{"token":""}`, `not json`} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(body))
		}))
		path := filepath.Join(t.TempDir(), "token")
		if err := fetchBrokerToken(context.Background(), srv.URL, path); err == nil {
			t.Errorf("%q was accepted", body)
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("%q left a file behind", body)
		}
		srv.Close()
	}
}

// The write replaces atomically: a reader that opens the file mid-refresh gets
// the old token or the new one, never half of either.
func TestReplacingATokenIsAtomic(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	if err := writeToken(path, "pbt1.first"); err != nil {
		t.Fatalf("first: %v", err)
	}
	if err := writeToken(path, "pbt1.second-and-longer"); err != nil {
		t.Fatalf("second: %v", err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(body) != "pbt1.second-and-longer" {
		t.Errorf("token = %q", body)
	}
	// The temporary file must not survive: a directory accumulating .new files
	// is a directory somebody has to explain later.
	if _, err := os.Stat(path + ".new"); !os.IsNotExist(err) {
		t.Error("the temporary file was left behind")
	}
}

// A machine with no broker in its environment starts nothing, and says nothing
// on every poke about it.
func TestNoBrokerInTheEnvironmentStartsNoLoop(t *testing.T) {
	t.Setenv("PILOT_BROKER_URL", "")
	t.Setenv("PILOT_TOKEN_FILE", "")
	// Nothing to assert but that it returns: the value of this test is that it
	// does not panic or block, which is what a poke on a pre-broker machine
	// must do.
	startBrokerRefresh()
}
