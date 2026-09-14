package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

const ioFlusherHelperEnv = "PILOTS_TEST_IOFLUSHER_HELPER"

// The kernel's own bits for what PR_SET_IO_FLUSHER sets (include/linux/sched.h).
const (
	pfMemallocNoIO  = 0x00080000
	pfLocalThrottle = 0x00100000
)

// The mark is per thread, so a handler that set it without re-executing would
// report success from the one thread that called prctl while its goroutines
// ran on threads that still deadlock. What has to hold is that EVERY thread of
// the process that goes on to serve carries it.
func TestBecomeIOFlusherMarksEveryThread(t *testing.T) {
	if os.Getenv(ioFlusherHelperEnv) == "1" {
		becomeIOFlusher()
		// Force more threads into existence than the runtime starts with,
		// since they are the ones a prctl without the re-exec would miss.
		for range 8 {
			go func() {
				runtime.LockOSThread()
				select {}
			}()
		}
		runtime.Gosched()
		tasks, _ := filepath.Glob("/proc/self/task/*/stat")
		for _, task := range tasks {
			b, _ := os.ReadFile(task)
			s := string(b)
			fields := strings.Fields(s[strings.LastIndex(s, ")")+2:])
			flags, _ := strconv.ParseUint(fields[6], 10, 64)
			fmt.Printf("%s %d\n", filepath.Base(filepath.Dir(task)), flags)
		}
		os.Exit(0)
	}
	if os.Geteuid() != 0 {
		t.Skip("PR_SET_IO_FLUSHER needs CAP_SYS_RESOURCE; run as root to exercise it")
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestBecomeIOFlusherMarksEveryThread$")
	cmd.Env = append(os.Environ(), ioFlusherHelperEnv+"=1")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("helper: %v", err)
	}
	lines := strings.Fields(strings.TrimSpace(string(out)))
	if len(lines) < 4 {
		t.Fatalf("helper reported too few threads to mean anything: %q", out)
	}
	for i := 0; i+1 < len(lines); i += 2 {
		flags, _ := strconv.ParseUint(lines[i+1], 10, 64)
		if flags&pfLocalThrottle == 0 || flags&pfMemallocNoIO == 0 {
			t.Errorf("thread %s is not an I/O flusher (flags %#x)", lines[i], flags)
		}
	}
}
