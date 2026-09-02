package github

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

func sign(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// The endpoint is public, so the signature is the only thing standing between
// a stranger and a deploy of their choosing.
func TestOnlyAGenuineSignatureIsAccepted(t *testing.T) {
	body := []byte(`{"ref":"refs/heads/main"}`)

	if err := Verify("s3cret", body, sign("s3cret", body)); err != nil {
		t.Errorf("a genuine delivery was rejected: %v", err)
	}
	if err := Verify("s3cret", body, sign("wrong-key", body)); err == nil {
		t.Error("a delivery signed with the wrong secret was accepted")
	}
	if err := Verify("s3cret", []byte(`{"ref":"refs/heads/evil"}`), sign("s3cret", body)); err == nil {
		t.Error("a tampered body kept its old signature and was accepted")
	}
	if err := Verify("", body, sign("s3cret", body)); err == nil {
		t.Error("a fleet with no configured secret accepted a delivery")
	}
	if err := Verify("s3cret", body, "sha1=abcdef"); err == nil {
		t.Error("a sha1 signature header was accepted")
	}
	if err := Verify("s3cret", body, "sha256=not-hex"); err == nil {
		t.Error("a non-hex signature was accepted")
	}
}

// GitHub wraps a repository in one directory named for the commit. A context
// handed to the builder unstripped has no Dockerfile at its root, and the
// build fails with a message about a missing file rather than the wrapper.
func TestTheTarballWrapperIsStripped(t *testing.T) {
	cases := map[string]string{
		"owner-repo-abc1234/Dockerfile":     "Dockerfile",
		"owner-repo-abc1234/src/index.js":   "src/index.js",
		"owner-repo-abc1234/a/b/c/deep.txt": "a/b/c/deep.txt",
	}
	for in, want := range cases {
		got, ok := StripRoot(in)
		if !ok || got != want {
			t.Errorf("StripRoot(%q) = %q, %v; want %q", in, got, ok, want)
		}
	}
	// The wrapper's own entry has nothing under it and is dropped.
	if _, ok := StripRoot("owner-repo-abc1234/"); ok {
		t.Error("the wrapper directory entry was kept as a real path")
	}
	if _, ok := StripRoot("no-slash"); ok {
		t.Error("an entry with no wrapper was accepted")
	}
}

// A push to a tag or a note is not a branch push and must not deploy.
func TestOnlyBranchRefsCountAsPushes(t *testing.T) {
	if got := (Event{Ref: "refs/heads/main"}).Branch(); got != "main" {
		t.Errorf("branch = %q, want main", got)
	}
	if got := (Event{Ref: "refs/tags/v1.0.0"}).Branch(); got != "" {
		t.Errorf("a tag push reported branch %q; it would have deployed", got)
	}
}
