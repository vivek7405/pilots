package cli

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/vivek7405/pilots/cli/internal/config"
	"github.com/vivek7405/pilots/cli/internal/out"
)

var secretName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_-]*$`)

// secretStore is the `secrets` half of the credentials file: app -> name ->
// value. It lives in the 0600 file `pilot login` writes, and nothing here
// ever prints a value: `ls` shows a digest, so two machines can be compared
// without either revealing anything.
type secretStore map[string]map[string]string

func loadSecrets(getenv config.Env) (*config.Credentials, secretStore, error) {
	creds, err := config.Load(getenv)
	if err != nil {
		return nil, nil, err
	}
	if creds == nil {
		creds = &config.Credentials{}
	}
	store := secretStore{}
	if len(creds.Secrets) > 0 {
		if err := json.Unmarshal(creds.Secrets, &store); err != nil {
			return nil, nil, fmt.Errorf("the secrets in the credentials file are not app -> name -> value: %w", err)
		}
	}
	return creds, store, nil
}

func saveSecrets(getenv config.Env, creds *config.Credentials, store secretStore) error {
	raw, err := json.Marshal(store)
	if err != nil {
		return err
	}
	creds.Secrets = raw
	return config.Save(getenv, creds)
}

// appFromCompose reads the top-level `name:` of a compose file, which is
// what the plan derives the app from. Anything more than that is the host's
// job, so this does not parse YAML beyond one line.
func appFromCompose(dir, file string) (string, error) {
	path := file
	if path == "" {
		path = findComposeFile(dir)
	} else if !filepath.IsAbs(path) {
		path = filepath.Join(dir, path)
	}
	if path == "" {
		return "", out.Failf("pass --app", "no compose file in %s to take the app from (looked for %s)", dir, strings.Join(composeFileNames, ", "))
	}
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "name:") {
			return strings.Trim(strings.TrimSpace(strings.TrimPrefix(line, "name:")), `"'`), nil
		}
	}
	return "", out.Failf("add a top-level name: to the compose file, or pass --app", "%s has no top-level name", path)
}

func newSecretsCmd(env *Env, getenv config.Env) *cobra.Command {
	var (
		app  string
		dir  string
		file string
	)
	scope := func(c *cobra.Command) *cobra.Command {
		c.Flags().StringVar(&app, "app", "", "the app the secrets belong to (default: the compose file's name)")
		c.Flags().StringVar(&dir, "dir", ".", "the directory holding the compose file")
		c.Flags().StringVar(&file, "file", "", "use this compose file instead of searching")
		return c
	}
	appFor := func() (string, error) {
		if app != "" {
			return app, nil
		}
		abs, err := filepath.Abs(dir)
		if err != nil {
			return "", err
		}
		return appFromCompose(abs, file)
	}

	root := &cobra.Command{
		Use:     "secrets",
		Aliases: []string{"secret"},
		Short:   "the values secret:// references resolve to, on this machine",
	}
	Describe(root, Doc{
		What: "A compose file says `secret://db-password`; the value lives here, in\n" +
			"the 0600 credentials file on this machine, and `pilot deploy` seals\n" +
			"it into the service's environment. The value never appears in a\n" +
			"compose file, a log line, or a `pilot secrets ls`.",
		Related: []string{
			"pilot services set --secret-env   set sealed values on a service directly",
			"pilot deploy                      where the references resolve",
		},
	})

	set := scope(&cobra.Command{
		Use:   "set <name> [value]",
		Short: "store one secret; prompts for the value when it is omitted",
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(c *cobra.Command, args []string) error {
			a, err := appFor()
			if err != nil {
				return err
			}
			name := args[0]
			if !secretName.MatchString(name) {
				return out.Failf("letters, digits, _ and -, not starting with a digit", "%q is not a secret name", name)
			}
			var value string
			if len(args) == 2 {
				value = args[1]
			} else if value, err = promptSecret(fmt.Sprintf("value for %s: ", name), "or pass the value as a second argument"); err != nil {
				return err
			}
			creds, store, err := loadSecrets(getenv)
			if err != nil {
				return err
			}
			if store[a] == nil {
				store[a] = map[string]string{}
			}
			store[a][name] = value
			if err := saveSecrets(getenv, creds, store); err != nil {
				return err
			}
			path, _ := config.Path(getenv)
			if env.W.JSON {
				return env.W.JSONValue(map[string]string{"app": a, "name": name})
			}
			env.W.Notef("stored %s for %s in %s", name, a, path)
			return nil
		},
	})
	Describe(set, Doc{
		Examples: []string{
			"pilot secrets set db-password",
			"pilot secrets set --app shop db-password 's3cret'",
		},
		Notes: "Passing the value as an argument puts it in your shell history;\n" +
			"omit it and type it at the prompt when that matters.",
	})

	imp := scope(&cobra.Command{
		Use:   "import <file>",
		Short: "store every KEY=value in a .env file (- reads stdin)",
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			a, err := appFor()
			if err != nil {
				return err
			}
			var r io.Reader = os.Stdin
			if args[0] != "-" {
				f, err := os.Open(args[0])
				if err != nil {
					return err
				}
				defer f.Close()
				r = f
			}
			creds, store, err := loadSecrets(getenv)
			if err != nil {
				return err
			}
			if store[a] == nil {
				store[a] = map[string]string{}
			}
			var names []string
			sc := bufio.NewScanner(r)
			for sc.Scan() {
				line := strings.TrimSpace(sc.Text())
				if line == "" || strings.HasPrefix(line, "#") {
					continue
				}
				line = strings.TrimPrefix(line, "export ")
				k, v, ok := strings.Cut(line, "=")
				k = strings.TrimSpace(k)
				if !ok || !secretName.MatchString(k) {
					return out.Failf("every line must be KEY=value", "not a KEY=value line: %q", line)
				}
				store[a][k] = strings.Trim(strings.TrimSpace(v), `"'`)
				names = append(names, k)
			}
			if err := saveSecrets(getenv, creds, store); err != nil {
				return err
			}
			if env.W.JSON {
				return env.W.JSONValue(map[string]any{"app": a, "names": names})
			}
			env.W.Notef("stored %d secrets for %s: %s", len(names), a, strings.Join(names, ", "))
			return nil
		},
	})
	Describe(imp, Doc{Examples: []string{"pilot secrets import .env.production", "cat .env | pilot secrets import -"}})

	ls := scope(&cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "secret names and digests for an app; never values",
		Args:    cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			a, err := appFor()
			if err != nil {
				return err
			}
			_, store, err := loadSecrets(getenv)
			if err != nil {
				return err
			}
			names := make([]string, 0, len(store[a]))
			for n := range store[a] {
				names = append(names, n)
			}
			sort.Strings(names)
			type entry struct {
				Name   string `json:"name"`
				Digest string `json:"digest"`
			}
			list := make([]entry, 0, len(names))
			rows := make([][]string, 0, len(names))
			for _, n := range names {
				sum := sha256.Sum256([]byte(store[a][n]))
				d := hex.EncodeToString(sum[:])[:12]
				list = append(list, entry{n, d})
				rows = append(rows, []string{n, d})
			}
			if env.W.JSON {
				return env.W.JSONValue(map[string]any{"app": a, "secrets": list})
			}
			if len(rows) == 0 {
				env.W.Notef("no secrets for %s", a)
				return nil
			}
			return env.W.Table([]string{"NAME", "DIGEST"}, rows)
		},
	})
	Describe(ls, Doc{
		How: "The digest is the first twelve hex characters of a SHA-256 of the\n" +
			"value, so two machines can check they hold the same secret without\n" +
			"either showing it.",
		Examples: []string{"pilot secrets ls --app shop"},
	})

	rm := scope(&cobra.Command{
		Use:     "remove <name>",
		Aliases: []string{"rm", "unset"},
		Short:   "forget a secret on this machine",
		Args:    cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			a, err := appFor()
			if err != nil {
				return err
			}
			creds, store, err := loadSecrets(getenv)
			if err != nil {
				return err
			}
			if _, ok := store[a][args[0]]; !ok {
				return out.Failf("pilot secrets ls shows what is stored", "no secret %s for %s", args[0], a)
			}
			delete(store[a], args[0])
			if len(store[a]) == 0 {
				delete(store, a)
			}
			if err := saveSecrets(getenv, creds, store); err != nil {
				return err
			}
			if env.W.JSON {
				return env.W.JSONValue(map[string]string{"app": a, "removed": args[0]})
			}
			env.W.Notef("removed %s for %s", args[0], a)
			return nil
		},
	})
	Describe(rm, Doc{
		Notes: "This forgets the value here. A service already sealed with it keeps\n" +
			"it until the next deploy resolves references again.",
	})

	root.AddCommand(set, imp, ls, rm)
	return root
}
