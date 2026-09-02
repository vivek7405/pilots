// Package github turns a repository push into a deploy, and a pull request
// into a preview, without a CI service anywhere.
//
// The webhook is an ordinary hostd route behind the same wildcard DNS as
// everything else, so any host may receive a delivery. Exactly one acts on it:
// hash(repo) mod live_hosts picks the actor, the same deterministic-owner rule
// that decides who rescues a machine and who writes a service row. No queue,
// no leader, no coordinator.
package github

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// MaxBody bounds a delivery. GitHub caps payloads at 25MB; anything past that
// is not a delivery we sent for.
const MaxBody = 25 << 20

// Event is the slice of a webhook payload this package acts on.
type Event struct {
	Action string `json:"action"`
	Ref    string `json:"ref"`
	After  string `json:"after"`

	Repository struct {
		FullName      string `json:"full_name"`
		DefaultBranch string `json:"default_branch"`
	} `json:"repository"`

	PullRequest struct {
		Number int  `json:"number"`
		Merged bool `json:"merged"`
		Head   struct {
			SHA string `json:"sha"`
			Ref string `json:"ref"`
		} `json:"head"`
	} `json:"pull_request"`

	Installation struct {
		ID int64 `json:"id"`
	} `json:"installation"`
}

// Branch is the branch a push landed on, or "" when the ref is not a branch.
func (e Event) Branch() string {
	const prefix = "refs/heads/"
	if strings.HasPrefix(e.Ref, prefix) {
		return strings.TrimPrefix(e.Ref, prefix)
	}
	return ""
}

// Verify checks a delivery's signature against the shared secret.
//
// Over the RAW body, before any parsing: a signature computed over
// re-serialised JSON validates a document GitHub never sent, and key order
// alone is enough to make that a different document. Constant-time compare,
// because a byte-at-a-time one leaks the expected digest to anyone who can
// send deliveries -- which is anyone at all, since the endpoint is public.
func Verify(secret string, body []byte, header string) error {
	if secret == "" {
		return fmt.Errorf("github: no webhook secret is configured")
	}
	const prefix = "sha256="
	if !strings.HasPrefix(header, prefix) {
		return fmt.Errorf("github: signature header is not sha256")
	}
	want, err := hex.DecodeString(strings.TrimPrefix(header, prefix))
	if err != nil {
		return fmt.Errorf("github: signature is not hex: %w", err)
	}

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	if !hmac.Equal(mac.Sum(nil), want) {
		return fmt.Errorf("github: signature does not match")
	}
	return nil
}

// ReadDelivery verifies and parses one webhook request.
func ReadDelivery(r *http.Request, secret string) (string, Event, error) {
	body, err := io.ReadAll(io.LimitReader(r.Body, MaxBody))
	if err != nil {
		return "", Event{}, err
	}
	if err := Verify(secret, body, r.Header.Get("X-Hub-Signature-256")); err != nil {
		return "", Event{}, err
	}
	var e Event
	if err := json.Unmarshal(body, &e); err != nil {
		return "", Event{}, fmt.Errorf("github: delivery is not JSON: %w", err)
	}
	return r.Header.Get("X-GitHub-Event"), e, nil
}

// StripRoot removes the single top-level directory GitHub's tarball wraps a
// repository in.
//
// Every entry is prefixed {owner}-{repo}-{sha7}/, so a context handed to the
// builder unstripped has no Dockerfile at its root and the build fails with a
// message about a missing file rather than about the wrapper.
func StripRoot(name string) (string, bool) {
	i := strings.Index(name, "/")
	if i < 0 {
		return "", false
	}
	rest := name[i+1:]
	if rest == "" {
		return "", false
	}
	return rest, true
}
