package cli

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	pilots "github.com/vivek7405/pilots/sdks/go"

	"github.com/vivek7405/pilots/cli/internal/out"
)

// Turning one Postgres into a cluster, and back.
//
// # Why this edits the compose file rather than only calling the API
//
// The compose file is the thing that gets deployed, committed and reviewed. A
// conversion that only changed the fleet would be undone by the next `pilot
// deploy` from a file that still says one replica, and the person running that
// deploy would have no way to know they had just taken their database apart.
//
// So the file is edited first, printed, and then deployed. What is in git is
// what is running.
//
// # What is deliberately NOT automated
//
// Nothing here decides which node is primary, and nothing here reads or writes
// that decision. Patroni owns it, inside the machines, with its own consensus.
// `status` asks each node what it thinks it is; `switchover` asks Patroni to
// move; neither invents an answer.

func newDBHACmd(env *Env) *cobra.Command {
	c := &cobra.Command{
		Use:   "ha",
		Short: "run a Postgres as a cluster, or stop",
	}
	Describe(c, Doc{
		What: "Several Postgres machines with automatic failover, on Patroni, with\n" +
			"its own etcd beside them.",
		How: "Each machine gets its OWN volume and is placed on a different host\n" +
			"where the fleet has one, because replicas that share a host are\n" +
			"replicas that die together.\n\n" +
			"The address does not change. Every node runs a proxy on the published\n" +
			"port that follows whichever node is currently primary, so the\n" +
			"connection string your application holds keeps working across a\n" +
			"failover without knowing one happened.",
		Warning: "You operate this database. Patroni decides which node is primary and\n" +
			"pilots does not participate in that decision, which means the three in\n" +
			"the morning is yours. docs/honesty.md says exactly which half is whose.",
		Related: []string{
			"pilot db connect   a session on whichever node is primary",
			"pilot db restore   point-in-time recovery, from the leader's archive",
		},
	})
	c.AddCommand(
		newDBHAEnableCmd(env),
		newDBHAStatusCmd(env),
		newDBHADisableCmd(env),
	)
	return c
}

func newDBHAEnableCmd(env *Env) *cobra.Command {
	var (
		replicas int
		etcd     int
		file     string
	)
	c := &cobra.Command{
		Use:   "enable <service>",
		Short: "turn one Postgres into a cluster",
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			client, err := env.Client()
			if err != nil {
				return err
			}
			svc, err := resolveService(c.Context(), client, args[0])
			if err != nil {
				return err
			}
			if svc.Labels["pilot.engine"] != "postgres" {
				return out.Failf("`pilot add postgres` writes one that can",
					"%s is not a Postgres written by a recipe, so it cannot become a cluster",
					svc.Name)
			}

			frag, err := client.Recipes.HAFragment(c.Context(), svc.Name, replicas, etcd)
			if err != nil {
				return err
			}

			// Said BEFORE anything is written, because it is the part somebody
			// has to have read: this is several machines, it costs, and the
			// operating half is theirs.
			env.W.Notef("%s", frag.Statement)
			// The global -y is what skips this; a second flag here would be a
			// second answer to one question.
			if err := confirm(env, fmt.Sprintf(
				"Convert %s to %d nodes with %d etcd members?", svc.Name, replicas, etcd)); err != nil {
				return err
			}

			if err := spliceHA(file, svc.Name, frag); err != nil {
				return err
			}
			env.W.Linef("updated %s", file)
			env.W.Notef("review the diff, then `pilot deploy` to bring the cluster up")
			env.W.Notef("the address and the connection string do not change")
			return nil
		},
	}
	f := c.Flags()
	f.IntVar(&replicas, "replicas", 2, "how many Postgres machines")
	f.IntVar(&etcd, "etcd", 3, "how many etcd members; odd, 3 to 9")
	f.StringVarP(&file, "file", "f", "compose.yaml", "the compose file to edit")
	Describe(c, Doc{
		What: "Rewrite the compose file so this database runs as a Patroni cluster,\n" +
			"with its own etcd.",
		How: "The FILE is edited, not just the fleet. A conversion that only changed\n" +
			"what is running would be undone by the next deploy from a file that\n" +
			"still says one replica, and whoever ran that deploy would have no way\n" +
			"to know they had taken the database apart.\n\n" +
			"Two Postgres machines and three etcd members is the smallest sensible\n" +
			"cluster, which is five machines. etcd must be an odd number: an even\n" +
			"one has no majority it did not already have at one fewer member, so it\n" +
			"buys failure modes and no availability.",
		Examples: []string{
			"pilot db ha enable postgres",
			"pilot db ha enable postgres --replicas 3 --etcd 5",
		},
	})
	return c
}

func newDBHAStatusCmd(env *Env) *cobra.Command {
	c := &cobra.Command{
		Use:   "status <service>",
		Short: "which node is primary, and what the others think",
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			client, err := env.Client()
			if err != nil {
				return err
			}
			svc, err := resolveService(c.Context(), client, args[0])
			if err != nil {
				return err
			}
			nodes, err := haStatus(c.Context(), client, svc.ID)
			if err != nil {
				return err
			}
			if env.W.JSON {
				return env.W.JSONValue(nodes)
			}
			if len(nodes) == 0 {
				return out.Failf("`pilot db ha enable` turns one on",
					"%s is not running as a cluster", svc.Name)
			}

			rows := make([][]string, 0, len(nodes))
			for _, n := range nodes {
				rows = append(rows, []string{n.Name, n.Role, n.Note})
			}
			if err := env.W.Table([]string{"NODE", "ROLE", ""}, rows); err != nil {
				return err
			}
			// Said when it is true, because a cluster with no primary is the
			// one state where doing nothing is wrong.
			if !anyPrimary(nodes) {
				env.W.Notef("no node reports itself primary: the cluster is electing, " +
					"or it has lost quorum. Check the etcd service.")
			}
			return nil
		},
	}
	Describe(c, Doc{
		What: "Each node, what role it reports, and whether anything disagrees.",
		How: "Asked of every node in parallel rather than of one: a node that\n" +
			"believes it is primary while another also does is the thing worth\n" +
			"seeing, and asking one node could never show it.",
		Examples: []string{"pilot db ha status postgres"},
	})
	return c
}

func newDBHADisableCmd(env *Env) *cobra.Command {
	var (
		file string
	)
	c := &cobra.Command{
		Use:   "disable <service>",
		Short: "return a cluster to a single machine",
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			client, err := env.Client()
			if err != nil {
				return err
			}
			svc, err := resolveService(c.Context(), client, args[0])
			if err != nil {
				return err
			}
			nodes, err := haStatus(c.Context(), client, svc.ID)
			if err != nil {
				return err
			}

			// The surviving machine is ordinal 1, so ordinal 1 has to be the
			// one holding the current data. Disabling while another node is
			// primary would keep the machine that is BEHIND and destroy the
			// one that is ahead, which is data loss dressed as a scale-down.
			if leader := primaryOf(nodes); leader != "" && !isFirstOrdinal(leader) {
				return out.Failf(
					"move it first: pilot db ha switchover "+svc.Name+" --to "+firstOrdinalName(nodes),
					"%s is primary, and disabling keeps the first node: the data "+
						"that is ahead would be the data destroyed", leader)
			}

			env.W.Notef("this destroys every node but the first, and its etcd service, " +
				"and returns the database to one machine")
			if err := confirm(env, "Return "+svc.Name+" to a single machine?"); err != nil {
				return err
			}
			if err := unspliceHA(file, svc.Name); err != nil {
				return err
			}
			env.W.Linef("updated %s", file)
			env.W.Notef("review the diff, then `pilot deploy`")
			env.W.Notef("delete the etcd service afterwards: a deploy never removes "+
				"a service it no longer sees, so `pilot services rm %s-etcd`", svc.Name)
			return nil
		},
	}
	f := c.Flags()
	f.StringVarP(&file, "file", "f", "compose.yaml", "the compose file to edit")
	Describe(c, Doc{
		What:    "Rewrite the compose file back to one Postgres machine.",
		Warning: "Every node but the first is destroyed, with its volume.",
		How: "Refused while any other node is primary. Disabling keeps the FIRST\n" +
			"node, so disabling while a later one leads would keep the machine that\n" +
			"is behind and destroy the one that is ahead, which is data loss dressed\n" +
			"up as a scale-down.",
		Examples: []string{"pilot db ha disable postgres"},
	})
	return c
}

// HANode is one member's own account of itself.
type HANode struct {
	Name string `json:"name"`
	Role string `json:"role"`
	Note string `json:"note,omitempty"`
}

// haStatus asks every replica what it thinks it is.
//
// Every one, in sequence over exec, because the disagreement is the interesting
// case: two nodes that both believe they are primary is exactly what somebody
// runs this to find, and asking one node could never show it.
func haStatus(ctx context.Context, client *pilots.Client, serviceID string) ([]HANode, error) {
	machines, err := client.Machines.List(ctx)
	if err != nil {
		return nil, err
	}
	var out []HANode
	for i := range machines {
		m := &machines[i]
		if m.ServiceID != serviceID || m.State != "running" {
			continue
		}
		out = append(out, askPatroni(ctx, client, m))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// askPatroni reads one node's own view, from inside it.
//
// A node that cannot be asked is reported as unreachable rather than dropped:
// a node missing from a cluster listing reads as a cluster that is smaller than
// it is, which is the opposite of what somebody needs to see.
func askPatroni(ctx context.Context, client *pilots.Client, m *pilots.Machine) HANode {
	res, err := client.Machines.Exec(ctx, m.ID, pilots.ExecRequest{
		Cmd:  `curl -s -o /dev/null -w %{http_code} http://127.0.0.1:8008/primary`,
		User: "root",
	})
	if err != nil {
		return HANode{Name: m.Name, Role: "unknown", Note: "could not be asked: " + err.Error()}
	}
	switch strings.TrimSpace(res.Stdout) {
	case "200":
		return HANode{Name: m.Name, Role: "primary"}
	case "503":
		// Patroni's own answer for "I am not the primary", which on this
		// endpoint is a replica rather than a failure.
		return HANode{Name: m.Name, Role: "replica"}
	case "":
		return HANode{Name: m.Name, Role: "unknown", Note: "no answer from Patroni"}
	default:
		return HANode{Name: m.Name, Role: "unknown", Note: "Patroni answered " + strings.TrimSpace(res.Stdout)}
	}
}

func anyPrimary(nodes []HANode) bool { return primaryOf(nodes) != "" }

func primaryOf(nodes []HANode) string {
	for _, n := range nodes {
		if n.Role == "primary" {
			return n.Name
		}
	}
	return ""
}

// isFirstOrdinal reports whether a node name is the first ordinal's.
//
// By the NAME suffix, because that is what the rollout gives an ordinal and it
// is stable across a redeploy. A machine's position in a list is not: a list is
// whatever order the store returned.
func isFirstOrdinal(name string) bool {
	return strings.HasSuffix(name, "-1") || !strings.ContainsAny(name, "-")
}

func firstOrdinalName(nodes []HANode) string {
	for _, n := range nodes {
		if isFirstOrdinal(n.Name) {
			return n.Name
		}
	}
	if len(nodes) > 0 {
		return nodes[0].Name
	}
	return "the first node"
}
