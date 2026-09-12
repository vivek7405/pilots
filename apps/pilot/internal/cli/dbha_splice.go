package cli

import (
	"os"
	"sort"

	yaml "go.yaml.in/yaml/v3"

	pilots "github.com/vivek7405/pilots/sdks/go"

	"github.com/vivek7405/pilots/cli/internal/out"
)

// Editing somebody's compose file, and putting it back readable.
//
// Through `yaml.Node` rather than a decode and re-encode, for the reason
// `pilot add` does the same: a round trip through a map loses every comment and
// reorders every key, so a person reviewing the diff would be reviewing a file
// that had been rewritten rather than edited. What they should see is the four
// lines that changed.

// spliceHA writes the conversion into a compose file.
func spliceHA(path, name string, frag *pilots.ComposeHAFragment) error {
	root, doc, err := loadCompose(path)
	if err != nil {
		return err
	}

	services := mapValue(doc, "services")
	if services == nil {
		return out.Failf("run this where your compose file is",
			"%s has no services", path)
	}
	block := mapValue(services, name)
	if block == nil {
		return out.Failf("check the name in "+path,
			"%s has no service called %s", path, name)
	}

	// MERGED into the existing block rather than replacing it: the recipe's
	// own volumes, healthcheck and build context are what make this a database,
	// and a conversion that replaced the block would quietly drop them.
	for _, key := range sortedAnyKeys(frag.Service) {
		if err := setInNode(block, key, frag.Service[key]); err != nil {
			return err
		}
	}
	if mapValue(services, frag.EtcdName) != nil {
		return out.Failf("it is already there; `pilot deploy` brings it up",
			"%s already has a service called %s", path, frag.EtcdName)
	}
	if err := setInMap(doc, "services", frag.EtcdName, frag.Etcd); err != nil {
		return err
	}
	if err := setInMap(doc, "volumes", frag.EtcdVolume, map[string]any{}); err != nil {
		return err
	}
	return writeCompose(path, &root)
}

// unspliceHA returns a compose file to a single machine.
//
// The etcd SERVICE is left in the file and its removal is printed instead,
// because a deploy never deletes a service it no longer sees: removing the
// block here would leave the machines running with nothing in the file naming
// them, which is worse than a line somebody has to delete.
func unspliceHA(path, name string) error {
	root, doc, err := loadCompose(path)
	if err != nil {
		return err
	}
	services := mapValue(doc, "services")
	if services == nil {
		return out.Failf("run this where your compose file is", "%s has no services", path)
	}
	block := mapValue(services, name)
	if block == nil {
		return out.Failf("check the name in "+path, "%s has no service called %s", path, name)
	}

	if err := setInNode(block, "deploy", map[string]any{"replicas": 1}); err != nil {
		return err
	}
	// The role goes back to single, which is what starts plain Postgres on the
	// same data directory. Patroni's data directory IS a valid Postgres one,
	// which is why this is a restart rather than a restore.
	env := mapValue(block, "environment")
	if env != nil {
		if err := setInNode(env, "PILOT_PG_ROLE", "single"); err != nil {
			return err
		}
	}
	removeKey(block, "depends_on")
	return writeCompose(path, &root)
}

func loadCompose(path string) (yaml.Node, *yaml.Node, error) {
	var root yaml.Node
	body, err := os.ReadFile(path)
	if err != nil {
		return root, nil, out.Failf("run this where your compose file is",
			"could not read %s: %v", path, err)
	}
	if err := yaml.Unmarshal(body, &root); err != nil {
		return root, nil, out.Failf("fix the file, or move it aside",
			"%s is not valid YAML: %v", path, err)
	}
	if len(root.Content) == 0 {
		return root, nil, out.Failf("run this where your compose file is",
			"%s is empty", path)
	}
	return root, root.Content[0], nil
}

func writeCompose(path string, root *yaml.Node) error {
	rendered, err := yaml.Marshal(root)
	if err != nil {
		return err
	}
	return os.WriteFile(path, rendered, 0o644)
}

// setInNode sets one key on an existing mapping node, replacing it in place so
// the key keeps its position and its comment.
func setInNode(node *yaml.Node, key string, value any) error {
	var encoded yaml.Node
	if err := encoded.Encode(value); err != nil {
		return err
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			node.Content[i+1] = &encoded
			return nil
		}
	}
	node.Content = append(node.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Value: key}, &encoded)
	return nil
}

// removeKey drops one key and its value from a mapping node.
func removeKey(node *yaml.Node, key string) {
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			node.Content = append(node.Content[:i], node.Content[i+2:]...)
			return
		}
	}
}

// sortedAnyKeys keeps two runs of one command writing one file rather than two
// that differ only in line order.
func sortedAnyKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
