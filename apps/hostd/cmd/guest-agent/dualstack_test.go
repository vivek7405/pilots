package main

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"os"
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

	// A loopback-only listener is not fronted: the shim would otherwise hand
	// every peer an app that chose to be reachable from nowhere else.
	loopback := "  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode\n" +
		"   0: 0100007F:1F90 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 1 1 0000000000000000 100 0 0 10 0\n"
	if !tableHas(strings.NewReader(loopback), 8080, false) {
		t.Fatal("the loopback LISTEN socket was not seen at all")
	}
	if tableHas(strings.NewReader(loopback), 8080, true) {
		t.Fatal("a 127.0.0.1-only listener would be exposed to peers by the shim")
	}
	if !tableHas(strings.NewReader(table), 8080, true) {
		t.Fatal("the wildcard listener was not accepted as one")
	}
}

// The links every init makes under /dev: a shell's process substitution opens
// /dev/fd, which devtmpfs does not provide, and the postgres image's
// entrypoint died on it. An existing entry is left alone.
func TestInitLinksTheStandardDevEntries(t *testing.T) {
	dir := t.TempDir()
	links := map[string]string{
		dir + "/fd":     "/proc/self/fd",
		dir + "/stdout": "/proc/self/fd/1",
	}
	if err := os.WriteFile(dir+"/stdout", []byte("kept"), 0o644); err != nil {
		t.Fatal(err)
	}
	linkDevices(links)
	if got, err := os.Readlink(dir + "/fd"); err != nil || got != "/proc/self/fd" {
		t.Fatalf("fd -> %q, %v; want /proc/self/fd", got, err)
	}
	if b, _ := os.ReadFile(dir + "/stdout"); string(b) != "kept" {
		t.Fatal("an existing /dev entry was replaced")
	}
}

// A volume mounts empty: the lost+found that mke2fs leaves, and that the
// host's forced fsck puts back before every attach, is removed -- because
// initdb refuses a data directory that contains it. One that fsck has put
// recovered files into is the user's data, and stays.
func TestAVolumeMountsWithoutAnEmptyLostAndFound(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(dir+"/lost+found", 0o700); err != nil {
		t.Fatal(err)
	}
	clearEmptyLostAndFound(dir)
	if _, err := os.Stat(dir + "/lost+found"); !os.IsNotExist(err) {
		t.Fatal("an empty lost+found survived the mount")
	}
	clearEmptyLostAndFound(dir) // absent is fine

	if err := os.MkdirAll(dir+"/lost+found/#12", 0o700); err != nil {
		t.Fatal(err)
	}
	clearEmptyLostAndFound(dir)
	if _, err := os.Stat(dir + "/lost+found/#12"); err != nil {
		t.Fatalf("a lost+found holding recovered files was removed: %v", err)
	}
}

// A client that half-closes after its request still gets the answer: the
// shim passes the EOF on as a half-close and keeps the other direction open.
func TestTheShimCarriesAHalfClose(t *testing.T) {
	app, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	go func() {
		c, err := app.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		req, _ := io.ReadAll(c) // reads until the client's EOF
		_, _ = c.Write([]byte("answer to " + string(req)))
	}()

	front, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer front.Close()
	go func() {
		c, err := front.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		up, err := net.Dial("tcp", app.Addr().String())
		if err != nil {
			return
		}
		defer up.Close()
		pipe(c, up)
	}()

	client, err := net.Dial("tcp", front.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	_, _ = client.Write([]byte("ping"))
	_ = client.(*net.TCPConn).CloseWrite()
	_ = client.SetReadDeadline(time.Now().Add(3 * time.Second))
	got, err := io.ReadAll(client)
	if err != nil {
		t.Fatalf("reading the answer: %v", err)
	}
	if string(got) != "answer to ping" {
		t.Fatalf("got %q, want the answer written after the client's half-close", got)
	}
}
