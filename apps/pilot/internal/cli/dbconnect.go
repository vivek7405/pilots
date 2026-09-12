package cli

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	pilots "github.com/vivek7405/pilots/sdks/go"

	"github.com/vivek7405/pilots/cli/internal/config"
	"github.com/vivek7405/pilots/cli/internal/out"
)

// A shell on your database, from your own terminal.
//
// # Why the engine's own client and not one we write
//
// Everybody who operates a Postgres already knows psql, and any query tool we
// wrote would be a worse psql that also had to be maintained. So this opens the
// path and gets out of the way: a TCP tunnel to the machine on 127.0.0.1, then
// the client that is already installed, pointed at it.
//
// # Two ways in, and why both exist
//
// LOCAL runs the client on this machine, over a tunnel. It is the better one:
// your own history file, your own pager, your own ~/.psqlrc, and a paste that
// does not cross a terminal stream.
//
// REMOTE runs the client inside the machine, over the console. It is what makes
// the command work for somebody who has neither the client installed nor the
// password -- a teammate who never ran `pilot add`. The password is already in
// that machine's environment, so nothing has to be fetched to make it work.
//
// The choice is made by what is actually available, and printed, so nobody has
// to wonder which one they got.

// engineClient is one engine's local client and how to point it at an address.
type engineClient struct {
	// Bin is the local binary, and Image is what to run inside the machine.
	// They are the same name for three of the four engines and different for
	// Mongo, whose image carries mongosh under the same name only since 6.
	Bin   string
	Image string
	// Port is where the engine listens inside the machine.
	Port int
	// PoolPort is where a pooler listens, when the recipe added one. Connecting
	// goes DIRECT by default: a session that cannot use a temporary table or
	// LISTEN is a surprising shell, and an interactive session is exactly the
	// one connection that does not need pooling.
	PoolPort int
	// Args builds the argv for an address and a user.
	Args func(host string, port int, user, database string) []string
	// PasswordEnv is the variable the client reads a password from, so it never
	// appears in a process list. Every one of these four has one, which is why
	// this works at all.
	PasswordEnv string
	// SecretName is the local secret holding the connection URL, as written by
	// `pilot add`.
	SecretName string
}

func clientFor(engine string) *engineClient {
	switch engine {
	case "postgres":
		return &engineClient{
			Bin: "psql", Image: "psql", Port: 5432, PoolPort: 6432,
			Args: func(host string, port int, user, database string) []string {
				return []string{"psql", "-h", host, "-p", strconv.Itoa(port),
					"-U", user, "-d", database}
			},
			PasswordEnv: "PGPASSWORD", SecretName: "postgres_url",
		}
	case "mysql":
		return &engineClient{
			Bin: "mysql", Image: "mysql", Port: 3306,
			Args: func(host string, port int, user, database string) []string {
				argv := []string{"mysql", "-h", host, "-P", strconv.Itoa(port), "-u", user}
				if database != "" {
					argv = append(argv, database)
				}
				return argv
			},
			PasswordEnv: "MYSQL_PWD", SecretName: "mysql_url",
		}
	case "redis":
		return &engineClient{
			Bin: "redis-cli", Image: "redis-cli", Port: 6379,
			Args: func(host string, port int, _, _ string) []string {
				return []string{"redis-cli", "-h", host, "-p", strconv.Itoa(port)}
			},
			PasswordEnv: "REDISCLI_AUTH", SecretName: "redis_url",
		}
	case "mongo":
		return &engineClient{
			Bin: "mongosh", Image: "mongosh", Port: 27017,
			Args: func(host string, port int, user, database string) []string {
				target := "mongodb://" + net.JoinHostPort(host, strconv.Itoa(port))
				if database != "" {
					target += "/" + database
				}
				argv := []string{"mongosh", "--quiet", target}
				if user != "" {
					argv = append(argv, "-u", user, "--authenticationDatabase", "admin")
				}
				return argv
			},
			PasswordEnv: "MONGODB_PASSWORD", SecretName: "mongo_url",
		}
	}
	return nil
}

// knownEngines is the list an error prints, in a stable order.
func knownEngines() []string {
	out := []string{"mongo", "mysql", "postgres", "redis"}
	sort.Strings(out)
	return out
}

func newDBConnectCmd(env *Env, getenv config.Env) *cobra.Command {
	var (
		engine   string
		port     int
		urlOnly  bool
		useLocal bool
		remote   bool
		pooled   bool
	)
	c := &cobra.Command{
		Use:   "connect [service]",
		Short: "a shell on a database, using the engine's own client",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			if useLocal && remote {
				return out.Failf("pass one of them", "--local and --remote contradict")
			}
			client, err := env.Client()
			if err != nil {
				return err
			}
			svc, err := resolveDatabase(c.Context(), env, client, args)
			if err != nil {
				return err
			}
			if engine == "" {
				engine = svc.Labels["pilot.engine"]
			}
			if engine == "" {
				return out.Failf("pass --engine, or add it with `pilot add`",
					"%s carries no pilot.engine label, so there is no client to run", svc.Name)
			}
			eng := clientFor(engine)
			if eng == nil {
				return out.Failf("there are clients for "+strings.Join(knownEngines(), ", "),
					"no client for engine %q", engine)
			}
			target := eng.Port
			if pooled && eng.PoolPort != 0 {
				target = eng.PoolPort
			}
			if port != 0 {
				target = port
			}

			// Credentials come from the local store, which is where `pilot add`
			// put them. Nothing is fetched from the fleet here, even though a
			// route exists, because the fallback is better than a fetch: with no
			// local password the client runs INSIDE the machine, where the
			// password already is. That path needs no credential to move at all,
			// so moving one would be work done for no gain.
			_, store, err := loadSecrets(getenv)
			if err != nil {
				return err
			}
			raw := findDatabaseURL(store, eng.SecretName)
			user, database, password := "", "", ""
			if raw != "" {
				user, database, password = splitDatabaseURL(raw)
			}

			if urlOnly {
				if raw == "" {
					return out.Failf("run `pilot add` in this project, or read it from the dashboard",
						"no stored connection string for %s", svc.Name)
				}
				env.W.Linef("%s", raw)
				return nil
			}

			m, err := engineReplica(c.Context(), client, svc.ID)
			if err != nil {
				return err
			}

			// The ladder, and it is about what is ACTUALLY available rather
			// than about a preference. Local needs both the client binary and
			// the password; missing either, the machine has both already.
			local := !remote
			if local && !useLocal {
				if _, err := exec.LookPath(eng.Bin); err != nil {
					env.W.Notef("no %s on this machine, so the client runs inside %s instead",
						eng.Bin, m.Name)
					local = false
				} else if password == "" {
					env.W.Notef("no stored password for %s, so the client runs inside %s "+
						"instead, where it is already in the environment", svc.Name, m.Name)
					local = false
				}
			}
			if useLocal {
				if _, err := exec.LookPath(eng.Bin); err != nil {
					return out.Failf("install it, or drop --local to run it inside the machine",
						"--local needs %s on this machine and there is none", eng.Bin)
				}
				if password == "" {
					return out.Failf("run `pilot add` in this project, or drop --local",
						"--local needs the password and none is stored for %s", svc.Name)
				}
			}

			if !local {
				return connectInsideMachine(c, env, client, m, eng, target, database)
			}
			return connectOverTunnel(c, env, client, m, eng, target, user, database, password)
		},
	}
	f := c.Flags()
	f.StringVar(&engine, "engine", "", "override the engine, when the service carries no label")
	f.IntVar(&port, "port", 0, "connect to another port inside the machine")
	f.BoolVar(&urlOnly, "url", false, "print the connection string and exit")
	f.BoolVar(&useLocal, "local", false, "require the local client; fail rather than fall back")
	f.BoolVar(&remote, "remote", false, "run the client inside the machine")
	f.BoolVar(&pooled, "pooled", false, "connect through the pooler instead of direct")
	Describe(c, Doc{
		What: "An interactive session on a database, in the client you already\n" +
			"know: psql, mysql, redis-cli or mongosh.",
		When: "Whenever you would otherwise reach for a tunnel and a connection\n" +
			"string. This is those two steps as one.",
		How: "By default the client runs HERE, over a tunnel to the machine, so you\n" +
			"keep your own history, your own pager and your own psqlrc. If the\n" +
			"client is not installed, or this machine holds no password for the\n" +
			"database, it runs INSIDE the machine instead, where the password is\n" +
			"already in the environment. Which one you got is printed.\n\n" +
			"The connection goes DIRECT rather than through the pooler, even on a\n" +
			"pooled database. Transaction pooling costs temporary tables, session\n" +
			"advisory locks and LISTEN, which makes for a surprising shell, and an\n" +
			"interactive session is the one connection that does not need pooling.\n" +
			"Use --pooled to see what the application sees.\n\n" +
			"Passwords are read from the local credentials file. With none stored,\n" +
			"the client runs inside the machine instead, where the password already\n" +
			"is, so nothing has to be fetched to make this work.",
		Examples: []string{
			"pilot db connect",
			"pilot db connect postgres",
			"pilot db connect postgres --pooled",
			"pilot db connect --url",
		},
		Related: []string{
			"pilot proxy      a port, held open, for a tool that is not a shell",
			"pilot metrics    what the engine says about itself",
			"pilot db restore recover to a moment in the past",
		},
	})
	return c
}

// resolveDatabase finds the database to connect to.
//
// The ladder is deliberately short: the argument, then the only database in the
// org. Guessing past that -- picking one of several because it sorts first --
// is how somebody ends up typing DELETE into production while reading staging.
func resolveDatabase(ctx context.Context, env *Env, client *pilots.Client, args []string) (*pilots.Service, error) {
	if len(args) == 1 {
		return resolveService(ctx, client, args[0])
	}
	services, err := client.Services.List(ctx)
	if err != nil {
		return nil, err
	}
	var databases []pilots.Service
	for _, s := range services {
		if s.Labels["pilot.engine"] != "" {
			databases = append(databases, s)
		}
	}
	switch len(databases) {
	case 0:
		return nil, out.Failf("`pilot add postgres` adds one", "no database here to connect to")
	case 1:
		env.W.Notef("connecting to %s, the only database here", databases[0].Name)
		return &databases[0], nil
	}
	names := make([]string, 0, len(databases))
	for _, d := range databases {
		names = append(names, d.Name)
	}
	sort.Strings(names)
	return nil, out.Failf("name one: "+strings.Join(names, ", "),
		"there are %d databases here", len(databases))
}

// findDatabaseURL looks for one engine's stored URL across every app.
//
// Across apps because the command is run from wherever somebody happens to be,
// and a connection string is per database rather than per directory. The DIRECT
// one wins where both exist: this opens an interactive session, which is the
// connection that should not go through a transaction pooler.
func findDatabaseURL(store secretStore, name string) string {
	apps := make([]string, 0, len(store))
	for app := range store {
		apps = append(apps, app)
	}
	sort.Strings(apps)
	for _, app := range apps {
		if v := store[app][name+"_direct"]; v != "" {
			return v
		}
	}
	for _, app := range apps {
		if v := store[app][name]; v != "" {
			return v
		}
	}
	return ""
}

// splitDatabaseURL pulls the user, database and password out of a stored URL.
//
// Tolerant on purpose: a URL it cannot read yields empty strings, and empty
// strings send the caller down the remote path, which needs none of them. A
// parse failure here must not be the thing that stops somebody connecting.
func splitDatabaseURL(raw string) (user, database, password string) {
	u, err := url.Parse(raw)
	if err != nil || u.User == nil {
		return "", "", ""
	}
	password, _ = u.User.Password()
	return u.User.Username(), strings.TrimPrefix(u.Path, "/"), password
}

// connectOverTunnel runs the local client against a listener bridged to the
// machine.
//
// The listener is bound to 127.0.0.1 on a port the kernel picks, which is what
// keeps it private to this machine: an address the kernel chose and nothing
// published is not something a neighbour finds.
func connectOverTunnel(c *cobra.Command, env *Env, client *pilots.Client,
	m *pilots.Machine, eng *engineClient, port int, user, database, password string) error {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("opening a local port to forward from: %w", err)
	}
	defer ln.Close()

	ctx, cancel := context.WithCancel(c.Context())
	defer cancel()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				tunnel, err := client.Machines.TCP(ctx, m.ID, port)
				if err != nil {
					env.W.Notef("%s:%d: %v", m.Name, port, err)
					return
				}
				defer tunnel.Close()
				pipe(conn, tunnel)
			}()
		}
	}()

	addr := ln.Addr().(*net.TCPAddr)
	env.W.Notef("%s on %s:%d, over a tunnel from this machine", eng.Bin, m.Name, port)
	argv := eng.Args("127.0.0.1", addr.Port, user, database)

	cmd := exec.CommandContext(c.Context(), argv[0], argv[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	// The password goes in the ENVIRONMENT, never in argv: argv is world
	// readable in /proc on most systems, and a password in a process list is a
	// password in everybody's shell history of `ps`.
	cmd.Env = append(os.Environ(), eng.PasswordEnv+"="+password)
	if err := cmd.Run(); err != nil {
		// The client's own exit status is its own business: a query that
		// failed, or a session somebody quit. Reporting it as a pilot failure
		// would be reporting somebody's typo as a platform error.
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return nil
		}
		return fmt.Errorf("running %s: %w", eng.Bin, err)
	}
	return nil
}

// connectInsideMachine runs the engine's own client in the guest.
//
// Nothing is passed to it: the password is already in that machine's
// environment, put there by the same deploy that started the database. That is
// what makes this path work for somebody who has never run `pilot add`.
func connectInsideMachine(c *cobra.Command, env *Env, client *pilots.Client,
	m *pilots.Machine, eng *engineClient, port int, database string) error {
	argv := eng.Args("127.0.0.1", port, "", database)
	argv[0] = eng.Image
	switch eng.SecretName {
	case "postgres_url":
		argv = append(argv, "-U", "postgres")
	case "mysql_url":
		argv = append(argv, "-u", "root")
	case "redis_url":
		// The variable the recipe already sets, expanded by the guest's shell
		// rather than here: the password must not pass through this process.
		argv = []string{"sh", "-lc", strings.Join(argv, " ") + ` -a "$REDIS_PASSWORD" --no-auth-warning`}
	case "mongo_url":
		argv = append(argv, "-u", "root", "--authenticationDatabase", "admin",
			"-p", "$MONGO_INITDB_ROOT_PASSWORD")
		argv = []string{"sh", "-lc", strings.Join(argv, " ")}
	}
	env.W.Notef("%s inside %s", eng.Image, m.Name)
	return runConsole(c, env, client, m.ID, argv)
}
