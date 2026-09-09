package cli

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"

	"github.com/spf13/cobra"

	pilots "github.com/vivek7405/pilots/sdks/go"

	"github.com/vivek7405/pilots/cli/internal/out"
)

// A mapping is one local port forwarded to one port inside the machine.
type mapping struct {
	local, remote int
}

func parseMapping(s string) (mapping, error) {
	l, r, ok := strings.Cut(s, ":")
	if !ok {
		r = l
	}
	local, err := strconv.Atoi(l)
	if err != nil || local < 0 || local > 65535 {
		return mapping{}, out.Failf("write a port, or local:remote", "%q is not a port", l)
	}
	remote, err := strconv.Atoi(r)
	if err != nil || remote < 1 || remote > 65535 {
		return mapping{}, out.Failf("write a port, or local:remote", "%q is not a port", r)
	}
	return mapping{local, remote}, nil
}

func newProxyCmd(env *Env) *cobra.Command {
	var (
		stdio   string
		machine string
	)
	c := &cobra.Command{
		Use:   "proxy <port>|<local:remote> [more...]",
		Short: "forward local ports to ports inside a machine",
		Args:  cobra.ArbitraryArgs,
		RunE: func(c *cobra.Command, args []string) error {
			client, err := env.Client()
			if err != nil {
				return err
			}
			m, err := machineArg(c, env, client, machine)
			if err != nil {
				return err
			}
			if stdio != "" {
				return runStdioTunnel(c.Context(), client, m.ID, stdio)
			}
			if len(args) == 0 {
				return out.Failf("pilot proxy 5432, or pilot proxy 15432:5432", "no ports given")
			}
			var maps []mapping
			for _, a := range args {
				mp, err := parseMapping(a)
				if err != nil {
					return err
				}
				maps = append(maps, mp)
			}
			return runProxy(c.Context(), env, client, m, maps)
		},
	}
	c.Flags().StringVarP(&stdio, "stdio", "W", "", "forward stdin and stdout to host:port inside the machine, for an ssh ProxyCommand")
	c.Flags().StringVarP(&machine, "machine", "m", "", "the machine; else the .pilot context or -s")
	Describe(c, Doc{
		When: "- `pilot proxy` -- reach ANY TCP port inside a machine from localhost,\n" +
			"  any protocol: Postgres, Redis, a debugger, ssh. Needs the CLI running.\n" +
			"- the machine's URL -- always-on HTTP to one port, no CLI needed.\n\n" +
			"Use proxy for local development against something inside a machine;\n" +
			"use the URL to share, or for webhooks.",
		How: "Each accepted local connection becomes one tunnel to the machine,\n" +
			"carried through hostd to the guest agent, which dials 127.0.0.1:port\n" +
			"there. Bytes only; nothing on the path understands the protocol. A\n" +
			"suspended machine is woken by the first connection.",
		Examples: []string{
			"pilot proxy 5432",
			"pilot proxy 15432:5432 6379",
			"pilot proxy 8080 -m scratch",
			"# ssh through the tunnel",
			"ssh -o ProxyCommand='pilot proxy -W %h:%p -m scratch' user@scratch",
		},
		Related: []string{
			"pilot url        the always-on HTTP address",
			"pilot console    a shell, when a port is not what you need",
		},
	})
	return c
}

func runProxy(ctx context.Context, env *Env, client *pilots.Client, m *pilots.Machine, maps []mapping) error {
	var listeners []net.Listener
	for _, mp := range maps {
		ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(mp.local)))
		if err != nil {
			for _, l := range listeners {
				l.Close()
			}
			return out.Failf("pick another local port with local:remote", "listen on 127.0.0.1:%d: %v", mp.local, err)
		}
		listeners = append(listeners, ln)
		env.W.Notef("forwarding localhost:%d -> %s:%d", ln.Addr().(*net.TCPAddr).Port, m.Name, mp.remote)
	}
	env.W.Notef("ctrl-c to stop")

	var wg sync.WaitGroup
	for i, ln := range listeners {
		remote := maps[i].remote
		wg.Add(1)
		go func(ln net.Listener) {
			defer wg.Done()
			for {
				local, err := ln.Accept()
				if err != nil {
					return
				}
				go func() {
					defer local.Close()
					tunnel, err := client.Machines.TCP(ctx, m.ID, remote)
					if err != nil {
						env.W.Notef("%s:%d: %v", m.Name, remote, err)
						return
					}
					defer tunnel.Close()
					pipe(local, tunnel)
				}()
			}
		}(ln)
	}
	<-ctx.Done()
	for _, ln := range listeners {
		ln.Close()
	}
	wg.Wait()
	return nil
}

// runStdioTunnel is -W: one tunnel on stdin/stdout, which is exactly the
// shape ssh's ProxyCommand expects.
func runStdioTunnel(ctx context.Context, client *pilots.Client, id, hostPort string) error {
	_, portStr, ok := strings.Cut(hostPort, ":")
	if !ok {
		portStr = hostPort
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 1 || port > 65535 {
		return out.Failf("write -W host:port", "%q is not a port", portStr)
	}
	tunnel, err := client.Machines.TCP(ctx, id, port)
	if err != nil {
		return err
	}
	defer tunnel.Close()
	// The session ends when the REMOTE side closes, not when stdin does: a
	// client that has sent its whole request and closed its write end is
	// still waiting for the answer, and ssh's ProxyCommand contract is the
	// same. stdin reaching EOF only stops the copy in that direction.
	remoteDone := make(chan struct{})
	go func() { _, _ = io.Copy(tunnel, os.Stdin) }()
	go func() { _, _ = io.Copy(os.Stdout, tunnel); close(remoteDone) }()
	select {
	case <-remoteDone:
	case <-ctx.Done():
	}
	return nil
}

// pipe copies both ways and returns when either side ends.
func pipe(a, b net.Conn) {
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(a, b); done <- struct{}{} }()
	go func() { _, _ = io.Copy(b, a); done <- struct{}{} }()
	<-done
}

var _ = fmt.Sprintf
