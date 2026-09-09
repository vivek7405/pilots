// Package cli assembles the command tree and the help it prints.
//
// The help is the product's front door, so it is built deliberately rather
// than left to cobra's default template. Two references, and the better half
// of each:
//
//   - sprite's per-command help TEACHES. It opens with what the thing is or
//     when to reach for it, warns before it destroys something, and ends with
//     worked examples and the neighbouring commands. A flag table alone
//     assumes the reader already knows what they want.
//   - flyctl's root help GROUPS. Twenty-odd commands in one alphabetical list
//     is a wall; under headings it is a map. sprite can skip this because it
//     has nine commands. pilots cannot: it is a sandbox tool AND a PaaS.
//
// The grouping is by TASK, not by noun. A person arrives wanting to "run
// something now" or "put something in production", not wanting "the machines
// noun". Grouping by noun would also quietly re-introduce the split this
// product exists to remove: fly needs two vocabularies because a sprite and a
// machine are different things there. Here they are one thing with two
// lifecycles, so there is one vocabulary and `promote` is the visible bridge.
package cli

import (
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/spf13/cobra"
)

// Group is a heading in the root help. Order is the order they are printed:
// the sandbox path first, because it is the one somebody runs in their first
// minute, then the production path, then everything that supports both.
type Group struct {
	ID    string
	Title string
}

var groups = []Group{
	{"sandbox", "Run something now"},
	{"deploy", "Ship it"},
	{"inspect", "See what is happening"},
	{"storage", "State that outlives a machine"},
	{"account", "Fleet, keys and context"},
	{"help", "Help and troubleshooting"},
}

// groupOf reads the group a command declared through its annotations.
func groupOf(c *cobra.Command) string {
	// cobra adds its completion command at Execute time, after the tree was
	// grouped, so it is placed here rather than left under "Other".
	if c.Name() == "completion" {
		return "help"
	}
	if c.Annotations == nil {
		return ""
	}
	return c.Annotations["group"]
}

// InGroup tags a command with the heading it appears under.
func InGroup(c *cobra.Command, id string) *cobra.Command {
	if c.Annotations == nil {
		c.Annotations = map[string]string{}
	}
	c.Annotations["group"] = id
	return c
}

// Doc is the teaching material a command carries beyond its one-line summary.
// Every field is optional; a command that needs none of them prints the same
// shape as before, just shorter.
type Doc struct {
	// What the thing IS, for a reader who has not met it yet.
	What string
	// When to reach for THIS command rather than its neighbours. This is the
	// section that stops somebody using `exec` when they wanted `console`.
	When string
	// How it works, when the mechanism changes what a caller should expect.
	How string
	// What is irreversibly lost. Printed under a WARNING heading, in full,
	// before the usage: a reader who stops reading early must still have seen
	// it. sprite's `destroy` help is the model.
	Warning string
	// Worked command lines. A comment line (starting #) is printed as-is.
	Examples []string
	// Commands worth knowing about next.
	Related []string
	// Anything else worth saying.
	Notes string
}

// docs holds each command's teaching material, keyed by the command itself, so
// a command's implementation is not buried under a page of prose. Keyed by
// pointer rather than by CommandPath because Describe runs before the command
// is attached to its parent, when its path is still just its own name.
var docs = map[*cobra.Command]Doc{}

// Describe attaches teaching material to a command.
func Describe(c *cobra.Command, d Doc) *cobra.Command {
	docs[c] = d
	return c
}

const indent = "  "

// writeSection prints a heading and a wrapped body, or nothing when empty.
func writeSection(w io.Writer, heading, body string) {
	body = strings.TrimSpace(body)
	if body == "" {
		return
	}
	fmt.Fprintf(w, "%s\n", heading)
	for _, line := range strings.Split(body, "\n") {
		if strings.TrimSpace(line) == "" {
			fmt.Fprintln(w)
			continue
		}
		fmt.Fprintf(w, "%s%s\n", indent, line)
	}
	fmt.Fprintln(w)
}

// rootHelp prints the grouped command map.
func rootHelp(c *cobra.Command, w io.Writer) {
	fmt.Fprintf(w, "%s\n\n", c.Short)
	if d, ok := docs[c]; ok {
		writeSection(w, "What pilots is", d.What)
	}
	fmt.Fprintf(w, "Usage\n%s%s\n\n", indent, "pilot [flags] <command> [arguments]")

	byGroup := map[string][]*cobra.Command{}
	var ungrouped []*cobra.Command
	for _, sub := range c.Commands() {
		if sub.Hidden || sub.Name() == "help" {
			continue
		}
		if g := groupOf(sub); g != "" {
			byGroup[g] = append(byGroup[g], sub)
			continue
		}
		ungrouped = append(ungrouped, sub)
	}

	width := 0
	for _, sub := range c.Commands() {
		if n := len(commandLabel(sub)); n > width {
			width = n
		}
	}

	for _, g := range groups {
		cmds := byGroup[g.ID]
		if len(cmds) == 0 {
			continue
		}
		sort.Slice(cmds, func(i, j int) bool { return cmds[i].Name() < cmds[j].Name() })
		fmt.Fprintf(w, "%s\n", g.Title)
		for _, sub := range cmds {
			fmt.Fprintf(w, "%s%-*s  %s\n", indent, width, commandLabel(sub), sub.Short)
		}
		fmt.Fprintln(w)
	}
	if len(ungrouped) > 0 {
		fmt.Fprintf(w, "Other\n")
		for _, sub := range ungrouped {
			fmt.Fprintf(w, "%s%-*s  %s\n", indent, width, commandLabel(sub), sub.Short)
		}
		fmt.Fprintln(w)
	}

	writeFlags(w, c)
	fmt.Fprintf(w, "Use 'pilot <command> --help' for what a command does, when to reach\nfor it, and worked examples.\n")
}

// commandLabel is the name plus its short alias, the way sprite prints
// "list (ls)": the alias is discoverable from the map rather than only from
// the command's own page.
func commandLabel(c *cobra.Command) string {
	short := ""
	for _, a := range c.Aliases {
		if len(a) < len(c.Name()) && (short == "" || len(a) < len(short)) {
			short = a
		}
	}
	if short == "" {
		return c.Name()
	}
	return fmt.Sprintf("%s (%s)", c.Name(), short)
}

// commandHelp prints one command's page: what it is, when to use it, what it
// destroys, how to call it, and what to read next.
func commandHelp(c *cobra.Command, w io.Writer) {
	fmt.Fprintf(w, "pilot %s - %s\n\n", c.CommandPath()[len("pilot "):], c.Short)

	d := docs[c]
	writeSection(w, "What it is", d.What)
	writeSection(w, "When to use it", d.When)
	writeSection(w, "How it works", d.How)
	// Before usage, deliberately: a reader who stops early has still seen it.
	writeSection(w, "WARNING", d.Warning)

	fmt.Fprintf(w, "Usage\n%s%s\n\n", indent, c.UseLine())

	if c.HasAvailableSubCommands() {
		fmt.Fprintf(w, "Commands\n")
		width := 0
		for _, sub := range c.Commands() {
			if n := len(commandLabel(sub)); n > width {
				width = n
			}
		}
		for _, sub := range c.Commands() {
			if sub.Hidden {
				continue
			}
			fmt.Fprintf(w, "%s%-*s  %s\n", indent, width, commandLabel(sub), sub.Short)
		}
		fmt.Fprintln(w)
	}

	writeFlags(w, c)

	if len(d.Examples) > 0 {
		fmt.Fprintf(w, "Examples\n")
		for _, ex := range d.Examples {
			fmt.Fprintf(w, "%s%s\n", indent, ex)
		}
		fmt.Fprintln(w)
	}
	writeSection(w, "Notes", d.Notes)
	if len(d.Related) > 0 {
		fmt.Fprintf(w, "Related\n")
		for _, r := range d.Related {
			fmt.Fprintf(w, "%s%s\n", indent, r)
		}
		fmt.Fprintln(w)
	}
}

// writeFlags prints a command's own flags, then the global ones separately.
// Separately because a reader scanning for what THIS command takes should not
// have to sift six flags every command shares.
func writeFlags(w io.Writer, c *cobra.Command) {
	// At the root every flag is a global flag, and cobra reports the
	// persistent set as "non-inherited" there, so printing both sections
	// would list the same six flags twice.
	if c.Root() == c {
		if f := c.PersistentFlags(); f.HasAvailableFlags() {
			fmt.Fprintf(w, "Global flags\n")
			fmt.Fprint(w, indentBlock(f.FlagUsages()))
			fmt.Fprintln(w)
		}
		return
	}
	if local := c.NonInheritedFlags(); local.HasAvailableFlags() {
		fmt.Fprintf(w, "Flags\n")
		fmt.Fprint(w, indentBlock(local.FlagUsages()))
		fmt.Fprintln(w)
	}
	if global := c.InheritedFlags(); global.HasAvailableFlags() {
		fmt.Fprintf(w, "Global flags\n")
		fmt.Fprint(w, indentBlock(global.FlagUsages()))
		fmt.Fprintln(w)
	}
}

// indentBlock re-indents pflag's own two-space output to this file's indent
// without re-implementing its column alignment.
func indentBlock(s string) string {
	var b strings.Builder
	for _, line := range strings.Split(strings.TrimRight(s, "\n"), "\n") {
		b.WriteString(strings.TrimRight(indent+strings.TrimPrefix(line, "  "), " "))
		b.WriteByte('\n')
	}
	return b.String()
}

// UseHelp installs the templates on a command tree.
func UseHelp(root *cobra.Command) {
	root.SetHelpFunc(func(c *cobra.Command, _ []string) {
		if c == c.Root() {
			rootHelp(c, c.OutOrStdout())
			return
		}
		commandHelp(c, c.OutOrStdout())
	})
	// A usage error prints the same page, so a mistyped command teaches too.
	root.SetUsageFunc(func(c *cobra.Command) error {
		if c == c.Root() {
			rootHelp(c, c.ErrOrStderr())
			return nil
		}
		commandHelp(c, c.ErrOrStderr())
		return nil
	})
}
