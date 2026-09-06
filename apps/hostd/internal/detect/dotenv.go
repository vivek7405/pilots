package detect

import (
	"bufio"
	"os"
	"strings"
)

// loadDotEnv reads a .env file into a map, or an empty map when there is none.
//
// hostd has no .env reader of its own: the CLI reads the file client-side with
// Node's own parser and posts the map. The plan route takes the whole
// directory as a tar instead, so the file arrives inside it and something here
// has to read it. Thirty lines rather than a dependency, and the table test
// mirrors the CLI's rows so the two parsers agree on what a quoted value is.
//
// Only the file. The host's own environment never reaches an interpolation: a
// deploy has to be reproducible from the checkout, and a plan that read
// whatever happened to be exported on one host would produce a different
// service on the next.
func loadDotEnv(path string) map[string]string {
	f, err := os.Open(path)
	if err != nil {
		return map[string]string{}
	}
	defer f.Close()

	out := map[string]string{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		key, value, ok := strings.Cut(line, "=")
		key = strings.TrimSpace(key)
		if !ok || key == "" {
			continue
		}
		out[key] = unquote(strings.TrimSpace(value))
	}
	return out
}

// unquote strips one matching pair of quotes. An unquoted value keeps
// everything up to a trailing comment, which is what every .env parser does
// and what a value like `KEY=a b # c` means to the person who wrote it.
func unquote(v string) string {
	if len(v) >= 2 {
		if (v[0] == '"' && v[len(v)-1] == '"') || (v[0] == '\'' && v[len(v)-1] == '\'') {
			return v[1 : len(v)-1]
		}
	}
	if i := strings.Index(v, " #"); i >= 0 {
		v = v[:i]
	}
	return strings.TrimSpace(v)
}
