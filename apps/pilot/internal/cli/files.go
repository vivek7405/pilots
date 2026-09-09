package cli

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	pilots "github.com/vivek7405/pilots/sdks/go"

	"github.com/vivek7405/pilots/cli/internal/out"
)

// Files move over the exec stream as a tar, in both directions. The stream
// carries stdin and stdout as binary frames already, so no new route is
// needed: a push is `tar x` on the machine fed from here, a pull is `tar c`
// on the machine read from here. One mechanism, and it is the one every
// other command already trusts.

// splitRemote parses machine:path. A bare path is local; machine is empty.
func splitRemote(arg string) (machine, p string) {
	if i := strings.Index(arg, ":"); i > 0 && !strings.HasPrefix(arg, "/") && !strings.HasPrefix(arg, ".") {
		return arg[:i], arg[i+1:]
	}
	return "", arg
}

// pushFiles copies a local file or directory to dest on the machine. dest
// is the path the source lands AT (a file) or IN (a directory), the way
// `cp` and `scp` read it.
func pushFiles(ctx context.Context, client *pilots.Client, id, src, dest string) error {
	info, err := os.Stat(src)
	if err != nil {
		return out.Failf("check the local path", "%v", err)
	}
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	base := path.Base(dest)
	destDir := path.Dir(dest)
	if info.IsDir() {
		// A directory lands as dest itself: its contents go under dest/.
		root := src
		err = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(root, p)
			name := base
			if rel != "." {
				name = path.Join(base, filepath.ToSlash(rel))
			}
			fi, err := d.Info()
			if err != nil {
				return err
			}
			hdr, err := tar.FileInfoHeader(fi, "")
			if err != nil {
				return err
			}
			hdr.Name = name
			if fi.IsDir() {
				hdr.Name += "/"
			}
			if err := tw.WriteHeader(hdr); err != nil {
				return err
			}
			if fi.Mode().IsRegular() {
				f, err := os.Open(p)
				if err != nil {
					return err
				}
				defer f.Close()
				_, err = io.Copy(tw, f)
				return err
			}
			return nil
		})
		if err != nil {
			return err
		}
	} else {
		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		hdr.Name = base
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		f, err := os.Open(src)
		if err != nil {
			return err
		}
		_, err = io.Copy(tw, f)
		f.Close()
		if err != nil {
			return err
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}

	cmd := fmt.Sprintf("mkdir -p %s && tar -x -C %s", shellQuote(destDir), shellQuote(destDir))
	stream, err := client.Machines.ExecStream(ctx, id, []string{"sh", "-c", cmd}, pilots.ExecStreamOptions{Stdin: true})
	if err != nil {
		return err
	}
	defer stream.Close()
	go func() {
		_, _ = io.Copy(stream.Stdin, &buf)
		_ = stream.Stdin.Close()
	}()
	_, stderr, code, err := stream.Output()
	if err != nil {
		return err
	}
	if code != 0 {
		return out.Failf("check the destination is writable by the machine's user", "tar on the machine exited %d: %s", code, strings.TrimSpace(string(stderr)))
	}
	return nil
}

// pullFiles copies src on the machine to a local dest.
func pullFiles(ctx context.Context, client *pilots.Client, id, src, dest string) error {
	cmd := fmt.Sprintf("cd %s && tar -c %s", shellQuote(path.Dir(src)), shellQuote(path.Base(src)))
	stream, err := client.Machines.ExecStream(ctx, id, []string{"sh", "-c", cmd}, pilots.ExecStreamOptions{Stdin: false})
	if err != nil {
		return err
	}
	defer stream.Close()
	stdout, stderr, code, err := stream.Output()
	if err != nil {
		return err
	}
	if code != 0 {
		return out.Failf("check the path exists on the machine", "tar on the machine exited %d: %s", code, strings.TrimSpace(string(stderr)))
	}
	// A destination that is an existing directory receives the file inside
	// it; otherwise the file lands at dest itself.
	intoDir := false
	if st, err := os.Stat(dest); err == nil && st.IsDir() {
		intoDir = true
	}
	tr := tar.NewReader(bytes.NewReader(stdout))
	base := path.Base(src)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		name := hdr.Name
		var target string
		if intoDir {
			target = filepath.Join(dest, filepath.FromSlash(name))
		} else if name == base || strings.HasPrefix(name, base+"/") {
			target = filepath.Join(dest, filepath.FromSlash(strings.TrimPrefix(name, base)))
		} else {
			target = filepath.Join(dest, filepath.FromSlash(name))
		}
		if !strings.HasPrefix(filepath.Clean(target), filepath.Clean(dest)) {
			return out.Failf("the archive tried to escape the destination", "refusing %s", name)
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, os.FileMode(hdr.Mode)&0o777|0o200)
			if err != nil {
				return err
			}
			_, err = io.Copy(f, tr)
			f.Close()
			if err != nil {
				return err
			}
		case tar.TypeSymlink:
			_ = os.Remove(target)
			if err := os.Symlink(hdr.Linkname, target); err != nil {
				return err
			}
		}
	}
	return nil
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func newFileCmd(env *Env) *cobra.Command {
	root := &cobra.Command{
		Use:   "file",
		Short: "copy files into and out of a machine, or edit one in place",
	}
	Describe(root, Doc{
		What: "push copies a local file or directory into a machine, pull copies one\n" +
			"out, edit opens a machine's file in $EDITOR and pushes it back. All\n" +
			"three travel over the same stream `pilot exec` uses, so nothing\n" +
			"extra has to be open on the machine.",
		Related: []string{
			"pilot exec   run a command there instead",
		},
	})

	push := &cobra.Command{
		Use:   "push <local> <machine>:<dest>",
		Short: "copy a local file or directory into a machine",
		Args:  cobra.ExactArgs(2),
		RunE: func(c *cobra.Command, args []string) error {
			client, err := env.Client()
			if err != nil {
				return err
			}
			name, dest := splitRemote(args[1])
			m, err := machineArg(c, env, client, name)
			if err != nil {
				return err
			}
			if err := pushFiles(c.Context(), client, m.ID, args[0], dest); err != nil {
				return err
			}
			if env.W.JSON {
				return env.W.JSONValue(map[string]string{"machine": m.ID, "dest": dest})
			}
			env.W.Notef("pushed %s to %s:%s", args[0], m.Name, dest)
			return nil
		},
	}
	Describe(push, Doc{
		Examples: []string{
			"pilot file push ./app.conf scratch:/etc/app.conf",
			"pilot file push ./dist scratch:/app/dist",
			"# with a .pilot context, the machine can be left out",
			"pilot file push .env :/app/.env",
		},
	})

	pull := &cobra.Command{
		Use:   "pull <machine>:<src> <local>",
		Short: "copy a file or directory out of a machine",
		Args:  cobra.ExactArgs(2),
		RunE: func(c *cobra.Command, args []string) error {
			client, err := env.Client()
			if err != nil {
				return err
			}
			name, src := splitRemote(args[0])
			m, err := machineArg(c, env, client, name)
			if err != nil {
				return err
			}
			if err := pullFiles(c.Context(), client, m.ID, src, args[1]); err != nil {
				return err
			}
			if env.W.JSON {
				return env.W.JSONValue(map[string]string{"machine": m.ID, "src": src, "dest": args[1]})
			}
			env.W.Notef("pulled %s:%s to %s", m.Name, src, args[1])
			return nil
		},
	}
	Describe(pull, Doc{
		Examples: []string{
			"pilot file pull scratch:/var/log/app.log ./logs/",
			"pilot file pull scratch:/app/build ./build",
		},
	})

	edit := &cobra.Command{
		Use:   "edit <machine>:<path>",
		Short: "edit a file on a machine in $EDITOR",
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			client, err := env.Client()
			if err != nil {
				return err
			}
			name, remote := splitRemote(args[0])
			m, err := machineArg(c, env, client, name)
			if err != nil {
				return err
			}
			editor := os.Getenv("VISUAL")
			if editor == "" {
				editor = os.Getenv("EDITOR")
			}
			if editor == "" {
				return out.Failf("set EDITOR, or pull, edit and push by hand", "no editor: neither VISUAL nor EDITOR is set")
			}
			tmpDir, err := os.MkdirTemp("", "pilot-edit-*")
			if err != nil {
				return err
			}
			defer os.RemoveAll(tmpDir)
			local := filepath.Join(tmpDir, path.Base(remote))
			if err := pullFiles(c.Context(), client, m.ID, remote, local); err != nil {
				return err
			}
			before, _ := os.ReadFile(local)
			cmd := exec.CommandContext(c.Context(), "sh", "-c", editor+" \"$1\"", "editor", local)
			cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
			if err := cmd.Run(); err != nil {
				return out.Failf("nothing was pushed", "%s exited: %v", editor, err)
			}
			after, err := os.ReadFile(local)
			if err != nil {
				return err
			}
			if bytes.Equal(before, after) {
				env.W.Notef("unchanged; nothing pushed")
				return nil
			}
			if err := pushFiles(c.Context(), client, m.ID, local, remote); err != nil {
				return err
			}
			env.W.Notef("pushed %s:%s", m.Name, remote)
			return nil
		},
	}
	Describe(edit, Doc{
		How: "The file is pulled to a temporary directory, opened in $VISUAL or\n" +
			"$EDITOR, and pushed back only if it changed.",
		Examples: []string{"pilot file edit scratch:/etc/app.conf"},
	})

	root.AddCommand(push, pull, edit)
	return root
}
