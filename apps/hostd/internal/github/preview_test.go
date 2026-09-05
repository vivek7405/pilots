package github

import (
	"strings"
	"testing"

	"github.com/vivek7405/pilots/hostd/internal/api"
)

// The comment on a pull request is the one place outside the API path where a
// client is handed a machine's URL, so it renders through the same
// api.PublicURL the handlers do. Before that it was a hardcoded https://,
// which on a single box listening plain on :8080 is a link that does not open.
func TestPreviewCommentRendersTheFleetsURLShape(t *testing.T) {
	const domain = "pr-7-demo.pilotrun.app"
	const sha = "0123456789abcdef"

	for _, tc := range []struct {
		name string
		url  api.PublicURL
		want string
	}{
		{
			// The pin: a TLS fleet renders exactly what this line always
			// rendered, byte for byte.
			name: "production",
			url:  api.PublicURLFor(true, ":8080"),
			want: "https://pr-7-demo.pilotrun.app",
		},
		{
			// A Deps that never set the field at all -- the zero value is the
			// production shape on purpose.
			name: "unset",
			want: "https://pr-7-demo.pilotrun.app",
		},
		{
			name: "single box on the plain listener",
			url:  api.PublicURLFor(false, "127.0.0.1:8080"),
			want: "http://pr-7-demo.pilotrun.app:8080",
		},
		{
			name: "plain on the port http already implies",
			url:  api.PublicURLFor(false, ":80"),
			want: "http://pr-7-demo.pilotrun.app",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := Deps{URL: tc.url}.previewComment(sha, domain)
			if !strings.Contains(body, ": "+tc.want+"\n") {
				t.Errorf("comment does not link %s:\n%s", tc.want, body)
			}
			// No second URL: a link the fleet cannot serve is worse than none.
			if strings.Count(body, "://") != 1 {
				t.Errorf("comment carries more than one URL:\n%s", body)
			}
		})
	}
}

// The whole comment, held to the byte on a TLS host: this is what push-to-deploy
// has always posted and this PR must not change it.
func TestPreviewCommentProductionShapeIsUnchanged(t *testing.T) {
	got := Deps{}.previewComment("0123456789abcdef", "pr-7-demo.pilotrun.app")
	want := "Preview for `0123456`: https://pr-7-demo.pilotrun.app\n\n" +
		"It suspends when idle and wakes on the next request, and is destroyed " +
		"when this pull request closes."
	if got != want {
		t.Errorf("preview comment changed shape\n got: %q\nwant: %q", got, want)
	}
}

// A short SHA is not truncated past its length. GitHub sends the full 40, but
// a redelivery replayed by hand has been shorter.
func TestPreviewCommentToleratesAShortSHA(t *testing.T) {
	if got := (Deps{}).previewComment("abc", "x.pilotrun.app"); !strings.Contains(got, "`abc`") {
		t.Errorf("short sha mangled: %q", got)
	}
}
