package cli

import (
	"archive/tar"
	"bytes"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/vivek7405/pilots/cli/internal/out"
)

// ignoreRule is one line of a .dockerignore: a pattern, and whether it
// re-includes (`!pattern`) rather than excludes.
type ignoreRule struct {
	pattern string
	negate  bool
}

// parseDockerignore reads rules in file order. Blank lines and comments are
// skipped; a leading `/` means the same as none, since every pattern is
// relative to the context root.
func parseDockerignore(text string) []ignoreRule {
	var rules []ignoreRule
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		negate := strings.HasPrefix(line, "!")
		if negate {
			line = strings.TrimSpace(line[1:])
		}
		line = strings.TrimPrefix(line, "/")
		line = path.Clean(line)
		if line == "." || line == "" {
			continue
		}
		rules = append(rules, ignoreRule{pattern: line, negate: negate})
	}
	return rules
}

// isIgnored applies the rules in order, last match wins, and a rule that
// matches any ANCESTOR of the path matches the path: excluding `node_modules`
// has to exclude everything under it, which is what .dockerignore means.
func isIgnored(rel string, rules []ignoreRule) bool {
	ignored := false
	for _, r := range rules {
		if matchesPathOrAncestor(rel, r.pattern) {
			ignored = !r.negate
		}
	}
	return ignored
}

func matchesPathOrAncestor(rel, pattern string) bool {
	// `**` at the start means "at any depth": match the basename anywhere.
	if strings.HasPrefix(pattern, "**/") {
		suffix := strings.TrimPrefix(pattern, "**/")
		parts := strings.Split(rel, "/")
		for i := range parts {
			if ok, _ := path.Match(suffix, strings.Join(parts[i:], "/")); ok {
				return true
			}
			if ok, _ := path.Match(suffix, parts[i]); ok {
				return true
			}
		}
		return false
	}
	for p := rel; p != "." && p != ""; p = path.Dir(p) {
		if ok, _ := path.Match(pattern, p); ok {
			return true
		}
	}
	return false
}

// mightBeReincluded says whether anything under an ignored directory could
// come back through a `!` rule, so the walk knows whether it may prune.
func mightBeReincluded(rel string, rules []ignoreRule) bool {
	for _, r := range rules {
		if r.negate && (strings.HasPrefix(r.pattern, rel+"/") || strings.HasPrefix(r.pattern, "**")) {
			return true
		}
	}
	return false
}

// tarDirectory packs a build context the way `docker build` would read it:
// .dockerignore honoured, `.git` never included, symlinks kept as symlinks
// rather than followed (one `node_modules/.bin` tree followed is a copy of
// every target), and paths sorted so the same tree packs to the same bytes,
// which is what lets the fleet's layer cache hit.
func tarDirectory(dir string) ([]byte, error) {
	var rules []ignoreRule
	if raw, err := os.ReadFile(filepath.Join(dir, ".dockerignore")); err == nil {
		rules = parseDockerignore(string(raw))
	}

	type entry struct {
		rel  string
		info fs.FileInfo
		link string
	}
	var entries []entry
	err := filepath.WalkDir(dir, func(full string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, full)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if rel == "." {
			return nil
		}
		if rel == ".git" || strings.HasPrefix(rel, ".git/") {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		ignored := isIgnored(rel, rules)
		if d.IsDir() {
			if ignored && !mightBeReincluded(rel, rules) {
				return filepath.SkipDir
			}
			if ignored {
				return nil
			}
		} else if ignored {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		e := entry{rel: rel, info: info}
		if info.Mode()&fs.ModeSymlink != 0 {
			if e.link, err = os.Readlink(full); err != nil {
				return err
			}
		}
		entries = append(entries, e)
		return nil
	})
	if err != nil {
		return nil, out.Failf("check the directory exists and is readable", "read %s: %v", dir, err)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].rel < entries[j].rel })

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range entries {
		hdr, err := tar.FileInfoHeader(e.info, e.link)
		if err != nil {
			return nil, err
		}
		hdr.Name = e.rel
		if e.info.IsDir() {
			hdr.Name += "/"
		}
		// Ownership and times are not part of the context's identity, and
		// leaving them in makes the same tree pack differently on two
		// machines, which defeats the layer cache.
		hdr.Uid, hdr.Gid, hdr.Uname, hdr.Gname = 0, 0, "", ""
		hdr.ModTime = hdr.ModTime.UTC().Truncate(0)
		hdr.AccessTime, hdr.ChangeTime = hdr.ModTime, hdr.ModTime
		if err := tw.WriteHeader(hdr); err != nil {
			return nil, out.Failf("a path over 255 bytes cannot be archived", "archive %s: %v", e.rel, err)
		}
		if e.info.Mode().IsRegular() {
			f, err := os.Open(filepath.Join(dir, filepath.FromSlash(e.rel)))
			if err != nil {
				return nil, err
			}
			_, err = io.Copy(tw, f)
			f.Close()
			if err != nil {
				return nil, err
			}
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
