package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestStatSessionIDSplitsFromTheLastParenthesis(t *testing.T) {
	for _, tc := range []struct {
		name string
		stat string
		want int
		ok   bool
	}{
		{"plain", "1234 (bash) S 1 1234 1234 34816 1234 4194560 0 0", 1234, true},
		{"comm with spaces", "77 (Web Content) S 1 77 4321 0 -1", 4321, true},
		{"comm with a parenthesis", "78 (a) b) R 1 78 99 0 -1", 99, true},
		{"truncated", "78 (sh) R 1", 0, false},
		{"no parenthesis", "garbage", 0, false},
		{"non-numeric session", "78 (sh) R 1 78 x 0", 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := statSessionID(tc.stat)
			if ok != tc.ok || got != tc.want {
				t.Errorf("statSessionID(%q) = %d, %v; want %d, %v", tc.stat, got, ok, tc.want, tc.ok)
			}
		})
	}
}

// A fixture process table: the leader alone is not busy, a second member is.
func TestSessionMembersReadsAFixtureProcTable(t *testing.T) {
	root := t.TempDir()
	write := func(pid int, stat string) {
		dir := filepath.Join(root, strconv.Itoa(pid))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "stat"), []byte(stat), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(100, "100 (bash) S 1 100 100 0 -1")     // the leader
	write(101, "101 (npm) S 100 101 100 0 -1")    // a child in the same session
	write(200, "200 (sshd) S 1 200 200 0 -1")     // another session entirely
	write(300, "300 (setsid) S 100 300 300 0 -1") // a child that daemonised
	// Not a pid, and a pid with no readable stat: both skipped.
	if err := os.MkdirAll(filepath.Join(root, "self"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "400"), 0o755); err != nil {
		t.Fatal(err)
	}

	got := sessionMembers(root, 100)
	if len(got) != 2 || got[0] != 100 || got[1] != 101 {
		t.Errorf("members of session 100 = %v, want [100 101]", got)
	}
	if got := sessionMembers(root, 300); len(got) != 1 || got[0] != 300 {
		t.Errorf("the daemonised child should be alone in its own session: %v", got)
	}
	if got := sessionMembers(filepath.Join(root, "missing"), 100); got != nil {
		t.Errorf("an unreadable table should answer nothing, got %v", got)
	}

	old := procRoot
	procRoot = root
	defer func() { procRoot = old }()
	if !sessionBusy(100) {
		t.Error("a session with a child besides the leader is busy")
	}
	if sessionBusy(200) {
		t.Error("a session that is only its leader is not busy")
	}
}

// The real thing, read from the live /proc: a setsid'd process alone in its
// session is idle, a setsid'd shell running a child is busy.
func TestSessionBusyAgainstTheLiveProcessTable(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("reads /proc")
	}
	if _, err := exec.LookPath("setsid"); err != nil {
		t.Skip("no setsid on this machine")
	}
	// setsid execs the program in place unless told to fork; --fork makes it
	// the parent and its child the session leader, and --wait keeps the
	// parent around so the pid we started from is still there to look under.
	start := func(args ...string) *exec.Cmd {
		cmd := exec.Command("setsid", append([]string{"--fork", "--wait"}, args...)...)
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
		return cmd
	}
	alone := start("sleep", "30")
	// "; true" keeps sh from exec'ing the sleep in place of itself, which
	// would leave the session with one member and no child to count.
	busy := start("sh", "-c", "sleep 30; true")
	time.Sleep(300 * time.Millisecond)

	// The leader is the process whose parent is the setsid we started.
	leaderOf := func(parent int) int {
		entries, _ := os.ReadDir("/proc")
		for _, e := range entries {
			pid, err := strconv.Atoi(e.Name())
			if err != nil {
				continue
			}
			raw, err := os.ReadFile(filepath.Join("/proc", e.Name(), "stat"))
			if err != nil {
				continue
			}
			stat := string(raw)
			fields := strings.Fields(stat[strings.LastIndexByte(stat, ')')+1:])
			if len(fields) >= 2 && fields[1] == strconv.Itoa(parent) {
				return pid
			}
		}
		return 0
	}
	if l := leaderOf(alone.Process.Pid); l == 0 || sessionBusy(l) {
		t.Errorf("a lone sleeping leader (pid %d) should not be busy", l)
	}
	if l := leaderOf(busy.Process.Pid); l == 0 || !sessionBusy(l) {
		t.Errorf("a shell running a child (pid %d) should be busy", l)
	}
}
