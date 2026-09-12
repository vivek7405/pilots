package cli

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	yaml "go.yaml.in/yaml/v3"

	"github.com/vivek7405/pilots/cli/internal/config"

	pilots "github.com/vivek7405/pilots/sdks/go"

	"github.com/vivek7405/pilots/cli/internal/out"
)

// Adding a database to a project.
//
// # What this is not
//
// It is not a managed database. Nobody here is on call for your Postgres, and
// `pilot add` does not pretend otherwise -- it prints what the mode costs and
// guarantees, because a durability decision the operator did not read is a
// decision they did not make.
//
// What it IS is the thing that makes a managed database useful nine times out
// of ten: the correct configuration, written into your own compose file, where
// you can read it, change it and keep it in version control.
//
// # Why it edits the file rather than hiding the database
//
// A database added here is an ordinary service. Same rollout, same health gate,
// same volume, same snapshots, same logs. Nothing about it is a second system
// with its own console, and nothing about it is invisible when something goes
// wrong at three in the morning.

func newAddCmd(env *Env, getenv config.Env) *cobra.Command {
	var (
		name          string
		durableVolume bool
		noPool        bool
		to            string
		dir           string
	)
	c := &cobra.Command{
		Use:       "add <postgres|mysql|redis|mongo>",
		Short:     "add a database to this project's compose file",
		Args:      cobra.ExactArgs(1),
		ValidArgs: []string{"postgres", "mysql", "redis", "mongo"},
		RunE: func(c *cobra.Command, args []string) error {
			engine := args[0]
			client, err := env.Client()
			if err != nil {
				return err
			}
			mode := ""
			if durableVolume {
				mode = "durable-volume"
			}
			recipe, err := client.Recipes.Get(c.Context(), engine, name, mode, !noPool)
			if err != nil {
				return err
			}
			svcName := name
			if svcName == "" {
				svcName = engine
			}

			root := dir
			if root == "" {
				root = "."
			}
			file, err := composeFileIn(root)
			if err != nil {
				return err
			}

			// The password is made HERE and stored locally. It never goes to
			// the fleet in this command: the recipe references it by name and
			// the deploy seals it on the way through.
			password, err := generatePassword()
			if err != nil {
				return err
			}

			if err := spliceRecipe(file, svcName, recipe); err != nil {
				return err
			}
			for path, body := range recipe.Files {
				full := filepath.Join(root, path)
				if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
					return err
				}
				mode := os.FileMode(0o644)
				if strings.HasSuffix(path, ".sh") {
					mode = 0o755
				}
				if err := os.WriteFile(full, []byte(body), mode); err != nil {
					return err
				}
			}

			// Stored under BOTH names: the password the container reads, and
			// the whole URL an application reads. An application that had to
			// assemble the URL itself would be a second place the port and the
			// scheme are written down.
			//
			// Secrets are per APP, because two projects on one machine must not
			// share a database password by accident.
			app, err := appFromCompose(root, filepath.Base(file))
			if err != nil {
				return err
			}
			creds, store, err := loadSecrets(getenv)
			if err != nil {
				return err
			}
			if store[app] == nil {
				store[app] = map[string]string{}
			}
			for _, n := range recipe.SecretNames {
				store[app][n] = password
			}
			store[app][engine+"_url"] = recipe.URLFor(password)
			if err := saveSecrets(getenv, creds, store); err != nil {
				return err
			}

			env.W.Linef("added %s to %s", svcName, file)
			for path := range sortedKeys(recipe.Files) {
				env.W.Linef("wrote %s", path)
			}
			// The durability statement, always, and not behind a flag. This is
			// the sentence that tells somebody what they just chose.
			env.W.Notef("%s", recipe.Statement)

			line := fmt.Sprintf("%s: secret://%s_url", recipe.ConnVar, engine)
			if to != "" {
				if err := addEnvTo(file, to, recipe.ConnVar, "secret://"+engine+"_url"); err != nil {
					return err
				}
				env.W.Linef("set %s on %s", recipe.ConnVar, to)
			} else {
				env.W.Notef("add this to whichever service uses it:\n  environment:\n    %s", line)
			}
			env.W.Notef("then `pilot deploy` to bring it up")
			// Said once, on the command that creates the expectation. A
			// platform that lets somebody believe their database is operated
			// for them has made the most expensive mistake available to it.
			env.W.Notef("you operate this database; we operate the platform it runs on. " +
				"docs/honesty.md says exactly which is which")
			return nil
		},
	}
	f := c.Flags()
	f.StringVar(&name, "name", "", "service name; defaults to the engine")
	f.BoolVar(&durableVolume, "durable-volume", false, "postgres: put the data directory on the volume (RPO 0, slower commits)")
	f.BoolVar(&noPool, "no-pool", false, "postgres: no connection pooler")
	f.StringVar(&to, "to", "", "also set the connection variable on this service")
	f.StringVar(&dir, "dir", "", "the project directory; defaults to the current one")
	Describe(c, Doc{
		What: "Writes a database into your compose file, with the configuration that\n" +
			"makes it survivable: a volume, a health gate, a private address, and a\n" +
			"daily snapshot schedule with retention.",
		When: "When an application needs a database. The fragment lands in your own\n" +
			"file, so you can read it, change it and commit it.",
		How: "The password is generated HERE and stored in your local secret store.\n" +
			"It never travels: the compose file references it by name, and the\n" +
			"deploy seals it on the way through.\n\n" +
			"Postgres defaults to shipping write-ahead log segments to a volume\n" +
			"every 60 seconds, which can lose up to a minute on a host failure and\n" +
			"keeps object storage out of the commit path. --durable-volume puts the\n" +
			"data directory itself on the volume: nothing is lost, and every commit\n" +
			"waits for object storage.",
		Warning: "This is not a managed database. Nobody here is on call for it. What you\n" +
			"get is the right configuration, written down where you can see it.",
		Examples: []string{
			"pilot add postgres",
			"pilot add postgres --to web",
			"pilot add postgres --durable-volume",
			"pilot add redis --name cache --to web",
		},
		Related: []string{
			"pilot deploy            bring it up",
			"pilot volumes policy    change the snapshot schedule",
		},
	})
	return c
}

// generatePassword makes one the URL and the shell can both carry.
//
// base64url rather than base64 or hex: the value goes into a connection URL AND
// into a shell environment, and `+`, `/` and `=` need escaping in both. 24
// bytes because that is 192 bits, which is past any argument about length.
func generatePassword() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", out.Failf("try again", "no entropy available to make a password: %v", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// composeFileIn finds the project's compose file, or names the one to make.
func composeFileIn(root string) (string, error) {
	for _, name := range []string{
		"compose.yaml", "compose.yml", "docker-compose.yaml", "docker-compose.yml",
	} {
		full := filepath.Join(root, name)
		if _, err := os.Stat(full); err == nil {
			return full, nil
		}
	}
	// None yet. compose.yaml is the name the spec prefers, so a project that
	// starts with a database starts with the right filename.
	return filepath.Join(root, "compose.yaml"), nil
}

// spliceRecipe writes the service and its volumes into the compose file,
// keeping everything else exactly as it was.
//
// Through yaml.Node rather than a decode-and-re-encode, which is the whole
// point: a round trip through map[string]any loses every comment and reorders
// every key, and somebody's compose file is a file they wrote.
func spliceRecipe(path, name string, recipe *pilots.ComposeRecipe) error {
	var root yaml.Node
	body, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if len(body) > 0 {
		if err := yaml.Unmarshal(body, &root); err != nil {
			return out.Failf("fix the file, or move it aside", "%s is not valid YAML: %v", path, err)
		}
	}
	if root.Kind == 0 {
		root = yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{
			{Kind: yaml.MappingNode},
		}}
	}
	doc := root.Content[0]

	if existing := mapValue(doc, "services"); existing != nil {
		if mapValue(existing, name) != nil {
			return out.Failf("pass --name to add it under another name",
				"%s already has a service called %s", path, name)
		}
	}
	if err := setInMap(doc, "services", name, recipe.Service); err != nil {
		return err
	}
	for vol := range recipe.Volumes {
		if err := setInMap(doc, "volumes", vol, map[string]any{}); err != nil {
			return err
		}
	}

	rendered, err := yaml.Marshal(&root)
	if err != nil {
		return err
	}
	return os.WriteFile(path, rendered, 0o644)
}

// addEnvTo sets one environment variable on an existing service.
func addEnvTo(path, service, key, value string) error {
	var root yaml.Node
	body, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := yaml.Unmarshal(body, &root); err != nil {
		return err
	}
	doc := root.Content[0]
	services := mapValue(doc, "services")
	if services == nil {
		return out.Failf("add the service first", "%s has no services", path)
	}
	target := mapValue(services, service)
	if target == nil {
		return out.Failf("check the name against the file", "%s has no service %q", path, service)
	}
	if err := setInMap(target, "environment", key, value); err != nil {
		return err
	}
	rendered, err := yaml.Marshal(&root)
	if err != nil {
		return err
	}
	return os.WriteFile(path, rendered, 0o644)
}

// mapValue is the value under a key in a mapping node, or nil.
func mapValue(node *yaml.Node, key string) *yaml.Node {
	if node == nil || node.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			return node.Content[i+1]
		}
	}
	return nil
}

// setInMap puts value under outer.inner, creating outer when it is absent.
func setInMap(node *yaml.Node, outer, inner string, value any) error {
	parent := mapValue(node, outer)
	if parent == nil {
		parent = &yaml.Node{Kind: yaml.MappingNode}
		node.Content = append(node.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Value: outer}, parent)
	}
	var encoded yaml.Node
	if err := encoded.Encode(value); err != nil {
		return err
	}
	// Replace an existing key rather than appending a duplicate: a mapping
	// with two of one key is valid YAML that every reader resolves differently.
	for i := 0; i+1 < len(parent.Content); i += 2 {
		if parent.Content[i].Value == inner {
			parent.Content[i+1] = &encoded
			return nil
		}
	}
	parent.Content = append(parent.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Value: inner}, &encoded)
	return nil
}

// sortedKeys iterates a map's keys in order, so two runs print the same thing.
func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
