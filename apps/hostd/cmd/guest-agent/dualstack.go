package main

import (
	"bufio"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"strings"
	"time"
)

// dualStackShim lets a peer reach an app that listens on IPv4 only.
//
// A machine's .internal name resolves to its mesh address, and the namespace
// rewrites that to the guest's IPv6 link address before the packet reaches
// eth0. So a peer's connection arrives over IPv6, and an app bound to
// 0.0.0.0:8080 -- which is what most frameworks do, and what the health
// gate's own advice says to do -- accepts nothing from it, while the router
// and the health probe, which dial the IPv4 link address, see it serve. The
// symptom was a name that resolved and never answered.
//
// When the app port has an IPv4 listener and no IPv6 one, this holds a
// v6-only socket on the same port and hands each connection to the app over
// loopback. It never takes the port ahead of the app: an app that binds
// dual-stack finds the port free, because the shim only binds once an
// IPv4-only listener exists, and lets go when that listener does.
func dualStackShim(port int) {
	for {
		if hasWildcardListener(procNetTCP4, port) && !hasListener(procNetTCP6, port) {
			ln, err := net.Listen("tcp6", fmt.Sprintf("[::]:%d", port))
			if err == nil {
				log.Printf("guest-agent: the app listens on IPv4 only; answering [::]:%d for peers", port)
				forwardUntilGone(ln, port)
				continue
			}
		}
		time.Sleep(2 * time.Second)
	}
}

const (
	procNetTCP4 = "/proc/net/tcp"
	procNetTCP6 = "/proc/net/tcp6"
)

// hasListener reports a LISTEN socket on the port in a /proc/net/tcp table.
func hasListener(table string, port int) bool {
	f, err := os.Open(table)
	if err != nil {
		return false
	}
	defer f.Close()
	return tableHasListener(f, port)
}

func tableHasListener(r io.Reader, port int) bool {
	return tableHas(r, port, false)
}

// hasWildcardListener is hasListener for a socket bound to every address.
//
// The shim exists for an app on 0.0.0.0 that a peer cannot reach over IPv6.
// An app on 127.0.0.1 chose to be reachable from nowhere else, and a shim on
// [::] forwarding to loopback would hand every peer a listener that was
// never meant to leave the guest. Only the wildcard bind is fronted.
func hasWildcardListener(table string, port int) bool {
	f, err := os.Open(table)
	if err != nil {
		return false
	}
	defer f.Close()
	return tableHas(f, port, true)
}

func tableHas(r io.Reader, port int, wildcardOnly bool) bool {
	want := fmt.Sprintf(":%04X", port)
	sc := bufio.NewScanner(r)
	sc.Scan() // header
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 4 {
			continue
		}
		if !strings.HasSuffix(fields[1], want) || fields[3] != "0A" {
			continue
		}
		if wildcardOnly && !strings.HasPrefix(fields[1], "00000000:") {
			continue
		}
		return true
	}
	return false
}

// forwardUntilGone serves the shim until the app's IPv4 listener goes away,
// then releases the port so a restarted app can bind it however it likes.
func forwardUntilGone(ln net.Listener, port int) {
	defer ln.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				app, err := net.DialTimeout("tcp4", fmt.Sprintf("127.0.0.1:%d", port), 5*time.Second)
				if err != nil {
					return
				}
				defer app.Close()
				pipe(conn, app)
			}()
		}
	}()
	for {
		time.Sleep(2 * time.Second)
		if !hasListener(procNetTCP4, port) {
			ln.Close()
			<-done
			return
		}
	}
}

// pipe copies both ways and returns when either side is done.
func pipe(a, b net.Conn) {
	errc := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(a, b); errc <- struct{}{} }()
	go func() { _, _ = io.Copy(b, a); errc <- struct{}{} }()
	<-errc
}
