package detect

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Workspaces expands npm's "workspaces" field to the directories that hold a
// package.json, sorted, relative to root.
//
// npm workspaces only, and that is a deliberate ceiling. It is the one
// monorepo convention that is declared in a file the platform already reads,
// with a glob syntax that is small enough to implement correctly; every other
// layout is a guess, and a guess that splits a repo into the wrong services is
// worse than answering "unknown" and letting the author write four lines of
// compose.
//
// Both spellings are accepted: an array of patterns, and the {"packages": []}
// object yarn popularised.
func Workspaces(root string) []string {
	raw, err := os.ReadFile(filepath.Join(root, "package.json"))
	if err != nil {
		return nil
	}
	var pkg struct {
		Workspaces json.RawMessage `json:"workspaces"`
	}
	if json.Unmarshal(raw, &pkg) != nil || len(pkg.Workspaces) == 0 {
		return nil
	}

	var patterns []string
	if json.Unmarshal(pkg.Workspaces, &patterns) != nil {
		var object struct {
			Packages []string `json:"packages"`
		}
		if json.Unmarshal(pkg.Workspaces, &object) != nil {
			return nil
		}
		patterns = object.Packages
	}

	seen := map[string]bool{}
	var out []string
	for _, pattern := range patterns {
		for _, dir := range expand(root, pattern) {
			if seen[dir] {
				continue
			}
			seen[dir] = true
			out = append(out, dir)
		}
	}
	sort.Strings(out)
	return out
}

// expand resolves one workspace pattern to directories holding a package.json.
//
// filepath.Glob covers the two shapes npm actually uses, "packages/*" and a
// literal directory. A pattern that escapes the root is dropped rather than
// clamped: the tar it came from is untrusted, and a workspace outside the
// context is not something the build could reach anyway.
func expand(root, pattern string) []string {
	pattern = strings.TrimSuffix(strings.TrimPrefix(filepath.Clean(pattern), "./"), "/")
	if pattern == "" || pattern == "." || strings.HasPrefix(pattern, "..") ||
		filepath.IsAbs(pattern) {
		return nil
	}
	matches, err := filepath.Glob(filepath.Join(root, pattern))
	if err != nil {
		return nil
	}
	var out []string
	for _, m := range matches {
		rel, err := filepath.Rel(root, m)
		if err != nil || strings.HasPrefix(rel, "..") {
			continue
		}
		info, err := os.Stat(m)
		if err != nil || !info.IsDir() {
			continue
		}
		if _, err := os.Stat(filepath.Join(m, "package.json")); err != nil {
			continue
		}
		out = append(out, filepath.ToSlash(rel))
	}
	return out
}
