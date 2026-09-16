package main

import (
	"bufio"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"
)

// An app bound to IPv4 only is reachable over IPv6 through the shim, which
// hands each connection to the app over loopback. This is what makes an app
// listening on 0.0.0.0 answer its .internal name, since peers arrive over the
// mesh's IPv6.
func TestAnIPv4OnlyAppAnswersOverIPv6ThroughTheShim(t *testing.T) {
	app, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Skip("no IPv4 loopback:", err)
	}
	defer app.Close()
	port := app.Addr().(*net.TCPAddr).Port
	go func() {
		for {
			c, err := app.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				line, _ := bufio.NewReader(c).ReadString('\n')
				fmt.Fprintf(c, "echo:%s", line)
			}()
		}
	}()

	shim, err := net.Listen("tcp6", fmt.Sprintf("[::1]:%d", port))
	if err != nil {
		t.Skip("no IPv6 loopback:", err)
	}
	go forwardUntilGone(shim, port)

	conn, err := net.DialTimeout("tcp6", fmt.Sprintf("[::1]:%d", port), 2*time.Second)
	if err != nil {
		t.Fatalf("a peer could not connect over IPv6: %v", err)
	}
	defer conn.Close()
	fmt.Fprintln(conn, "hello")
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	reply, _ := bufio.NewReader(conn).ReadString('\n')
	if reply != "echo:hello\n" {
		t.Fatalf("the app answered %q through the shim", reply)
	}
}

// The listener tables are read the way the kernel writes them: a LISTEN
// socket is state 0A on a local address ending in the hex port.
func TestListenerTableParsing(t *testing.T) {
	table := "  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode\n" +
		"   0: 00000000:1F90 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 1 1 0000000000000000 100 0 0 10 0\n" +
		"   1: 0100007F:0BB9 0100007F:B84A 01 00000000:00000000 02:000005C6 00000000     0        0 2 1 0000000000000000 20 4 30 10 -1\n"
	if !tableHasListener(strings.NewReader(table), 8080) {
		t.Fatal("the LISTEN socket on 8080 was not seen")
	}
	if tableHasListener(strings.NewReader(table), 3001) {
		t.Fatal("an ESTABLISHED socket on 3001 was taken for a listener")
	}
}
