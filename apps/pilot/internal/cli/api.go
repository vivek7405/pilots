package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/vivek7405/pilots/cli/internal/out"
)

func newAPICmd(env *Env) *cobra.Command {
	var (
		method string
		data   string
	)
	c := &cobra.Command{
		Use:   "api <path>",
		Short: "make an authenticated call to the fleet API",
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			if env.APIKey.Value == "" {
				return out.Failf("run pilot login, or set PILOT_API_KEY", "no API key for %s", env.APIURL.Value)
			}
			path := args[0]
			if !strings.HasPrefix(path, "/") {
				path = "/" + path
			}
			var body io.Reader
			switch {
			case data == "-":
				raw, err := io.ReadAll(os.Stdin)
				if err != nil {
					return err
				}
				body = bytes.NewReader(raw)
				if method == "" {
					method = http.MethodPost
				}
			case data != "":
				body = strings.NewReader(data)
				if method == "" {
					method = http.MethodPost
				}
			}
			if method == "" {
				method = http.MethodGet
			}
			req, err := http.NewRequestWithContext(c.Context(), strings.ToUpper(method), strings.TrimRight(env.APIURL.Value, "/")+path, body)
			if err != nil {
				return err
			}
			req.Header.Set("Authorization", "Bearer "+env.APIKey.Value)
			req.Header.Set("Accept", "application/json")
			if body != nil {
				req.Header.Set("Content-Type", "application/json")
			}
			if env.Org.Value != "" {
				q := req.URL.Query()
				q.Set("org", env.Org.Value)
				req.URL.RawQuery = q.Encode()
			}
			res, err := http.DefaultClient.Do(req)
			if err != nil {
				return err
			}
			defer res.Body.Close()
			raw, err := io.ReadAll(res.Body)
			if err != nil {
				return err
			}
			// The body is the answer whatever the status: a 4xx from the
			// fleet is exactly what the caller asked to see. Pretty-printed
			// when it is JSON, verbatim when it is not, and the status goes
			// to stderr so stdout stays parseable.
			env.W.Notef("HTTP %d", res.StatusCode)
			var pretty bytes.Buffer
			if json.Indent(&pretty, raw, "", "  ") == nil {
				pretty.WriteByte('\n')
				_, err = pretty.WriteTo(os.Stdout)
			} else {
				_, err = os.Stdout.Write(raw)
			}
			if err != nil {
				return err
			}
			if res.StatusCode >= 400 {
				return &ExitError{Code: 1}
			}
			return nil
		},
	}
	c.Flags().StringVarP(&method, "method", "X", "", "HTTP method (default GET, or POST when --data is given)")
	c.Flags().StringVarP(&data, "data", "d", "", "a JSON body; - reads it from stdin")
	Describe(c, Doc{
		When: "For the corner of the API a command does not cover yet, or to see\n" +
			"exactly what the fleet answers. The key, the fleet and the org come\n" +
			"from the same places every other command uses.",
		Examples: []string{
			"pilot api /v1/health",
			"pilot api /v1/machines | jq '.[].name'",
			"pilot api -X POST /v1/machines -d '{\"name\":\"scratch\"}'",
			"echo '{\"replicas\":2}' | pilot api -X PATCH /v1/services/svc_... -d -",
		},
		Notes: "The status line goes to stderr and the body to stdout, so the body\n" +
			"can be piped. A 4xx or 5xx exits 1 after printing the body.",
	})
	return c
}
