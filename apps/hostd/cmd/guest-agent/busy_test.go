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

// A fixture process table: one pass finds the sessions with a member besides
// their leader, and the rule on top of it tells a shell at its prompt from a
// leader that IS the job.
func TestBusySessionsAndTheRuleOnTopOfThem(t *testing.T) {
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
	write(100, "100 (bash) S 1 100 100 0 -1")     // a shell with a child
	write(101, "101 (npm) S 100 101 100 0 -1")    // the child, same session
	write(200, "200 (bash) S 1 200 200 0 -1")     // a shell alone at its prompt
	write(300, "300 (setsid) S 100 300 300 0 -1") // a child that daemonised: its own session
	write(400, "400 (python) S 1 400 400 0 -1")   // a tool run on a PTY with no shell
	// Not a pid, and a pid with no readable stat: both skipped.
	if err := os.MkdirAll(filepath.Join(root, "self"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "500"), 0o755); err != nil {
		t.Fatal(err)
	}

	busy := busySessions(root)
	if !busy[100] || busy[200] || busy[300] || busy[400] {
		t.Errorf("busySessions = %v, want only session 100 (the one with a child)", busy)
	}
	if got := busySessions(filepath.Join(root, "missing")); len(got) != 0 {
		t.Errorf("an unreadable table should answer nothing, got %v", got)
	}

	for _, tc := range []struct {
		name   string
		argv   []string
		leader int
		want   bool
	}{
		{"a shell running a command", []string{"/bin/sh"}, 100, true},
		{"a shell at its prompt", []string{"/bin/bash", "-l"}, 200, false},
		{"a daemonised child's own session, led by setsid", []string{"/bin/sh"}, 300, false},
		{"a tool that is the whole session", []string{"python", "train.py"}, 400, true},
		{"a tool with a busy shell's pid but not a shell", []string{"npm", "run", "build"}, 100, true},
		{"no leader recorded", []string{"/bin/sh"}, 0, false},
		{"no argv, no child", nil, 200, false},
	} {
		if got := sessionIsBusy(tc.argv, tc.leader, busy); got != tc.want {
			t.Errorf("%s: sessionIsBusy = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// The real thing, read from the live /proc: a setsid'd process alone in its
// session has no other member, a setsid'd shell running a child does.
func TestBusySessionsAgainstTheLiveProcessTable(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("reads /proc")
	}
	if _, err := exec.LookPath("setsid"); err != nil {
		t.Skip("no setsid on this machine")
	}
	// setsid execs the program in place unless told to fork; --fork makes it
	// the parent and its child the session leader, and --wait keeps the
	// parent around so the pid we started from is still there to look under.
	//
	// tail rather than sleep, deliberately: internal/fc's kill test scans
	// /proc for a detached session-leader named "sleep" that ignores SIGTERM,
	// and go test runs packages in parallel -- a sleep here was found by that
	// scan, killed by its SIGTERM, and reported as Kill "returning without
	// waiting". Two tests must not spawn the same process shape.
	start := func(args ...string) *exec.Cmd {
		cmd := exec.Command("setsid", append([]string{"--fork", "--wait"}, args...)...)
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
		return cmd
	}
	alone := start("tail", "-f", "/dev/null")
	// "; true" keeps sh from exec'ing the tail in place of itself, which
	// would leave the session with one member and no child to count.
	withChild := start("sh", "-c", "tail -f /dev/null; true")
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
	busy := busySessions(procRoot)
	if l := leaderOf(alone.Process.Pid); l == 0 || busy[l] {
		t.Errorf("a leader alone in its session (pid %d) has no other member", l)
	}
	if l := leaderOf(withChild.Process.Pid); l == 0 || !busy[l] {
		t.Errorf("a shell running a child (pid %d) should have a member besides itself", l)
	}
}
