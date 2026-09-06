package detect

import (
	"path/filepath"
	"testing"
)

// The rows mirror the CLI's own .env cases. Two parsers exist because the CLI
// reads the file client-side and this reads it out of the tar, and they have
// to agree on what a quoted value is: a secret that survives one and not the
// other is a deploy that works from the CLI and fails from the dashboard.
func TestLoadDotEnvMatchesTheCLIsRows(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, ".env", `# a comment
EMPTY=
PLAIN=value
SPACED = padded
QUOTED="a value"
SINGLE='a value'
HASH_INSIDE="a # b"
TRAILING=value # a trailing comment
EQUALS=a=b=c
export EXPORTED=yes

NOT A LINE
`)
	got := loadDotEnv(filepath.Join(dir, ".env"))
	want := map[string]string{
		"EMPTY":       "",
		"PLAIN":       "value",
		"SPACED":      "padded",
		"QUOTED":      "a value",
		"SINGLE":      "a value",
		"HASH_INSIDE": "a # b",
		"TRAILING":    "value",
		"EQUALS":      "a=b=c",
		"EXPORTED":    "yes",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
	if len(got) != len(want) {
		t.Errorf("read %d keys, want %d: %v", len(got), len(want), got)
	}
}

func TestLoadDotEnvOnAMissingFileIsEmptyRatherThanAnError(t *testing.T) {
	if got := loadDotEnv(filepath.Join(t.TempDir(), ".env")); len(got) != 0 {
		t.Errorf("loadDotEnv = %v, want an empty map", got)
	}
}
