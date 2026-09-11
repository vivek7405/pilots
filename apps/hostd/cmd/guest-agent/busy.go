package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Whether a session is doing anything, answered from the process tree rather
// than from its output.
//
// hostd counts a session as activity only while a client is attached: the
// websocket is what it can see. The guest can see more. Every tty session is
// its own Unix session (startPTY sets Setsid, so the shell is the session
// leader), and every process the shell starts -- a foreground command, a
// `&` job, a nohup'd child -- inherits that session id until it calls setsid
// itself. So "is a command running in this session" is exactly "is any
// process besides the leader alive with this session id", one pass over
// /proc, no heuristics about output.
//
// That is deliberately stricter than watching stdout. A build whose output
// goes to a file, or a tmux the client detached from, prints nothing to the
// PTY and is still busy. A shell sitting at a prompt is the one thing that is
// not, and a process that daemonised itself with setsid has left the session
// on purpose -- it is nobody's session, and the idle_timeout knob exists for
// that case.

// procRoot is where the process table is read from. A variable so a test can
// point it at a directory of fixtures.
var procRoot = "/proc"

// busySessions is one pass over the process table: the session ids that have
// at least one member besides their own leader. handleSessions asks once and
// answers every session from the result, rather than walking /proc once per
// session.
func busySessions(root string) map[int]bool {
	busy := map[int]bool{}
	eachProcess(root, func(pid, sid int) {
		if pid != sid {
			busy[sid] = true
		}
	})
	return busy
}

// sessionIsBusy is the rule for one live session, given what busySessions
// found: argv is what the session was started with, leader its pid.
//
// A session whose leader is a shell is busy while the shell has a child;
// alone, the shell is sitting at its prompt. A session whose leader is
// anything else -- `exec/stream?cmd=python&cmd=train.py&tty=true`, a tool
// run on a PTY with no shell around it -- IS the job, and is busy for as long
// as it is alive, which "is there a member besides the leader" would read as
// idle and suspend mid-run.
func sessionIsBusy(argv []string, leader int, busy map[int]bool) bool {
	if leader == 0 {
		return false
	}
	if len(argv) > 0 && !isShell(argv[0]) {
		return true
	}
	return busy[leader]
}

// isShell is whether a session leader is an interactive shell, which is the
// one kind of leader that can be alive and doing nothing.
func isShell(argv0 string) bool {
	switch filepath.Base(argv0) {
	case "sh", "bash", "dash", "zsh", "fish", "ash", "ksh":
		return true
	}
	return false
}

// eachProcess calls fn with the pid and session id of every readable entry
// under root.
//
// A read that fails for one entry -- the process exited between the listing
// and the read, or it is not ours to read -- skips that entry rather than
// failing the scan: a session with one unreadable member is still described
// correctly by its readable ones, and a scan that errors would have to answer
// "not busy", which is the answer that suspends a working machine.
func eachProcess(root string, fn func(pid, sid int)) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(root, e.Name(), "stat"))
		if err != nil {
			continue
		}
		if sid, ok := statSessionID(string(raw)); ok {
			fn(pid, sid)
		}
	}
}

// statSessionID reads the session id, field 6, out of one /proc/<pid>/stat
// line.
//
// Field 2 is the command name in parentheses and may itself contain spaces
// and parentheses ("(Web Content)", "(a) b)"), so the line is split from the
// LAST closing parenthesis, after which the fields are fixed: state, ppid,
// pgrp, session.
func statSessionID(stat string) (int, bool) {
	end := strings.LastIndexByte(stat, ')')
	if end < 0 {
		return 0, false
	}
	fields := strings.Fields(stat[end+1:])
	if len(fields) < 4 {
		return 0, false
	}
	sid, err := strconv.Atoi(fields[3])
	if err != nil {
		return 0, false
	}
	return sid, true
}
