package main

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// The create path replaces the token file and, where there is systemd,
// restarts the agent to pick it up. An image built from an ordinary Dockerfile
// has no systemd, so the agent has to notice on its own -- otherwise hostd
// holds a credential the guest never learned and every exec into a freshly
// built machine returns 401, on a machine that looks perfectly healthy.
func TestAuthPicksUpAReplacedToken(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "token")
	if err := os.WriteFile(path, []byte("old-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	restore := tokenPathForTest(t, path)
	defer restore()

	agentToken.Store("old-token")

	req := httptest.NewRequest("POST", "/exec", nil)
	req.Header.Set("Authorization", "Bearer new-token")
	if authOK(req) {
		t.Fatal("a token that is in neither memory nor the file was accepted")
	}

	if err := os.WriteFile(path, []byte("new-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !authOK(req) {
		t.Fatal("the agent did not pick up the replaced token, so every exec " +
			"into a freshly created machine would be rejected")
	}
	if got := currentToken(); got != "new-token" {
		t.Fatalf("cached token is %q after the reload", got)
	}

	// And the old one stops working, which is the other half of a replacement.
	old := httptest.NewRequest("POST", "/exec", nil)
	old.Header.Set("Authorization", "Bearer old-token")
	if authOK(old) {
		t.Fatal("the superseded token is still accepted")
	}
}

func TestAuthRejectsAnEmptyPresentedToken(t *testing.T) {
	agentToken.Store("something")
	if authOK(httptest.NewRequest("POST", "/exec", nil)) {
		t.Fatal("a request with no credential was accepted")
	}
}

// tokenPathForTest points the reload at a temporary file. tokenPath is a
// const in the binary because it is a fixed location inside the guest; this
// swaps the variable the reload actually reads.
func tokenPathForTest(t *testing.T, path string) func() {
	t.Helper()
	prev := tokenFile
	tokenFile = path
	return func() { tokenFile = prev }
}
