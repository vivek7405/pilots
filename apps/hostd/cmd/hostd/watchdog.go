package main

import (
	"context"
	"log/slog"
	"net"
	"os"
	"strconv"
	"time"

	"github.com/vivek7405/pilots/hostd/internal/metrics"
)

// The systemd watchdog, driven by whether hostd's loops are actually running.
//
// `Restart=always` restarts a process that dies. The failure this addresses is
// the one where nothing dies: a loop blocks on a query with no timeout or a
// socket that accepts and never answers, the process stays up, /v1/health
// answers 200, and the metering, the self-healing or the certificate renewal
// silently stops. Fly lost five hours of billing and renewal to that shape on
// 2026-09-02 with every process healthy.
//
// So the pet is conditional. Every third of WatchdogSec hostd sends WATCHDOG=1
// only while every registered loop is inside its budget; when one is not, the
// pet is WITHHELD and the loop is named in the log. systemd then restarts
// hostd, which is safe precisely here and nowhere else: the unit is
// KillMode=process, so the machines keep running and are re-adopted, and a
// restart costs a few seconds of API rather than an outage.
//
// No library. go.mod carries no systemd dependency, the socket protocol is one
// datagram, and notifyReady already writes it.

// sdNotify sends one datagram to systemd's notify socket. A host started by
// hand has no socket and no watchdog, which is exactly right for the local rig.
func sdNotify(msg string) {
	sock := os.Getenv("NOTIFY_SOCKET")
	if sock == "" {
		return
	}
	// A leading '@' denotes an abstract socket, written as a NUL byte.
	if sock[0] == '@' {
		sock = "\x00" + sock[1:]
	}
	conn, err := net.Dial("unixgram", sock)
	if err != nil {
		slog.Warn("sd_notify dial failed", "err", err)
		return
	}
	defer conn.Close()
	if _, err := conn.Write([]byte(msg)); err != nil {
		slog.Warn("sd_notify write failed", "err", err)
	}
}

// watchdogInterval is how often to pet, given systemd's deadline.
//
// A third of it, the interval systemd's own documentation recommends: two
// missed pets still leave one inside the window, so a single slow scheduling
// moment is not a restart.
func watchdogInterval(usec string) (time.Duration, bool) {
	n, err := strconv.ParseInt(usec, 10, 64)
	if err != nil || n <= 0 {
		return 0, false
	}
	d := time.Duration(n) * time.Microsecond / 3
	if d < time.Second {
		d = time.Second
	}
	return d, true
}

// runWatchdog pets systemd while hostd's loops are healthy.
//
// Returns immediately on a host with no watchdog configured, which is every
// host started outside systemd.
func runWatchdog(ctx context.Context) {
	every, ok := watchdogInterval(os.Getenv("WATCHDOG_USEC"))
	if !ok {
		return
	}
	slog.Info("watchdog armed", "pet_every", every)

	t := time.NewTicker(every)
	defer t.Stop()
	// Logged at most once per stall rather than on every pet, so a wedged loop
	// is one line and a restart, not a full journal.
	var warned bool
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if overdue := metrics.Overdue(); len(overdue) > 0 {
			if !warned {
				slog.Error("a background loop has stopped ticking; withholding the "+
					"systemd watchdog so this host is restarted. The machines keep "+
					"running: the unit is KillMode=process.", "loops", overdue)
				warned = true
			}
			continue
		}
		warned = false
		sdNotify("WATCHDOG=1\n")
	}
}
