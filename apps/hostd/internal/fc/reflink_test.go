package fc

import (
	"os"
	"os/exec"
	"testing"
)

// The probe must not report support it cannot demonstrate. Whichever
// filesystem the test runs on, the answer has to match what an actual
// --reflink=always copy does there -- that is the only thing the engine's
// timings depend on.
func TestSupportsReflinkMatchesRealClone(t *testing.T) {
	dir := t.TempDir()
	got := SupportsReflink(dir)

	src, err := os.CreateTemp(dir, "src-*")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := src.Write(make([]byte, 4096)); err != nil {
		t.Fatal(err)
	}
	src.Close()
	dst := src.Name() + ".clone"
	want := exec.Command("cp", "--reflink=always", src.Name(), dst).Run() == nil

	if got != want {
		t.Fatalf("SupportsReflink(%s) = %v, but a real --reflink=always copy %s",
			dir, got, map[bool]string{true: "succeeded", false: "failed"}[want])
	}
}

// The probe runs at startup on every host, so it must not leave anything
// behind: a probe file per boot in the machine store is a slow leak nobody
// would think to look for.
func TestSupportsReflinkLeavesNothingBehind(t *testing.T) {
	dir := t.TempDir()
	SupportsReflink(dir)

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("probe left %d file(s) behind: %v", len(entries), names)
	}
}
