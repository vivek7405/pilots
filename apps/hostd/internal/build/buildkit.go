package build

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"

	"github.com/vivek7405/pilots/hostd/internal/api"
)

// solveArgs builds the buildctl invocation.
//
// Two choices in here are decisions rather than defaults:
//
//   - `--output type=tar` and NOT type=oci. The tar exporter emits the
//     FLATTENED filesystem of the build result as a single archive, which is
//     what an ext4 image needs. The oci exporter emits a layered image
//     tarball, and unpacking those layers -- applying whiteouts, ordering
//     diffs -- is reimplementing a container runtime to arrive at the same
//     bytes the tar exporter already produced.
//   - `--progress rawjson`. The machine-readable stream. The alternative is
//     scraping a display that redraws itself, where the failing command's
//     output can be overwritten by the next frame.
//
// exportSuffix names the directory a cache is exported to before it replaces
// the one it was imported from.
const exportSuffix = ".new"

func (b *Builder) solveArgs(addr, contextDir, out, cacheDir, seedDir string) []string {
	args := []string{
		"--addr", addr,
		"build",
		"--frontend", "dockerfile.v0",
		"--local", "context=" + contextDir,
		"--local", "dockerfile=" + contextDir,
		"--output", "type=tar,dest=" + out,
		"--progress", "rawjson",
	}

	// The cache is what makes a redeploy cheap. Both directories are on the
	// HOST and reach the daemon over the buildctl session, so the guest is
	// never told a path it could reach on its own and never holds a
	// credential for one.
	if cacheDir != "" {
		args = append(args,
			// mode=max caches intermediate layers too, not just the result.
			//
			// Exported to a SIBLING of the directory it imports from, which
			// the caller then swaps into place. BuildKit's local exporter
			// writes an OCI layout and never collects what a new index
			// supersedes, so exporting over the import directory would leave
			// every generation's blobs behind: an org redeploying one
			// Dockerfile would grow this directory without bound on NVMe, in
			// the bucket it is mirrored to, and in the download a cold host
			// pays. One export, one generation.
			"--export-cache", "type=local,dest="+cacheDir+exportSuffix+",mode=max",
			"--import-cache", "type=local,src="+cacheDir)
	}
	// The shared seed is imported READ ONLY, and only hostd ever writes it.
	// An org importing another org's output would be a supply chain the
	// tenant chooses; an org importing bytes hostd built from a constant in
	// this package is not.
	if seedDir != "" {
		args = append(args, "--import-cache", "type=local,src="+seedDir)
	}
	return args
}

// solve runs BuildKit and streams its progress into the log contract.
//
// The exit status is the authority on whether the build worked. A rawjson
// stream that reported no vertex error but ended in a non-zero exit is still a
// failed build -- and a build that reports success while producing nothing is
// the failure mode that hangs a deploy, so the two are checked separately and
// both are surfaced.
func (b *Builder) solve(ctx context.Context, addr, contextDir, out, cacheDir string,
	record func(api.BuildLogLine)) error {

	args := b.solveArgs(addr, contextDir, out, cacheDir, b.seedDir())
	cmd := exec.CommandContext(ctx, b.opts.BuildctlBin, args...)
	// Its own process group, so a timeout kills the whole build tree rather
	// than leaving buildctl's children running against a daemon that has
	// stopped being watched.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// CommandContext's default cancel signals the process alone, which with
	// Setpgid is exactly the pid that is NOT the rest of the tree. Signal the
	// group, or a timed-out build leaves buildctl's children running.
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	// No storage credentials reach this process, and none reach the daemon.
	// The daemon runs inside a machine the ORG controls: given bucket
	// credentials it could write a cache manifest under any key, and the next
	// org whose Dockerfile hashed the same would import it. Everything the
	// build needs from object storage is fetched and pushed by hostd itself,
	// on this side of the guest boundary.

	// rawjson goes to stderr; buildctl writes nothing useful to stdout with a
	// file output.
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("build: pipe buildctl output: %w", err)
	}
	cmd.Stdout = io.Discard

	if err := cmd.Start(); err != nil {
		record(failure("build", err))
		return fmt.Errorf("build: start buildctl: %w", err)
	}

	parser := newProgressParser()
	parseErr := parser.Parse(stderr, record)

	waitErr := cmd.Wait()
	if waitErr != nil {
		// Name the step. An agent reading this stream to patch its own
		// Dockerfile and retry needs to know WHICH instruction failed, and the
		// exit status alone says only that something did.
		step, msg := parser.Failed, parser.FailMsg
		if step == "" {
			step = "build"
		}
		if msg == "" {
			msg = waitErr.Error()
		}
		err := fmt.Errorf("build failed at %s: %s", step, msg)
		record(failure(step, err))
		return err
	}
	if parseErr != nil {
		record(failure("build", parseErr))
		return parseErr
	}
	// A vertex error with a zero exit has been seen; trust the stream too.
	if parser.Failed != "" {
		err := fmt.Errorf("build failed at %s: %s", parser.Failed, parser.FailMsg)
		record(failure(parser.Failed, err))
		return err
	}

	if info, err := os.Stat(out); err != nil || info.Size() == 0 {
		err := fmt.Errorf("the build reported success but produced no filesystem")
		record(failure("build", err))
		return err
	}
	return nil
}

// cacheNameFor is the cache key a build imports from and exports to.
//
// Keyed on the Dockerfile's own content rather than on an app name, because
// this layer has no app: two deploys of the same Dockerfile share a cache
// wherever they run, and two different Dockerfiles never collide.
func cacheNameFor(contextDir string) string {
	raw, err := os.ReadFile(contextDir + "/Dockerfile")
	if err != nil {
		return ""
	}
	return "df-" + shortHash(raw)
}

func shortHash(b []byte) string {
	// FNV-1a, inline: this is a cache partition name, not a security boundary,
	// and pulling in a hash import for sixteen hex digits is not worth it.
	const (
		offset64 = 14695981039346656037
		prime64  = 1099511628211
	)
	var h uint64 = offset64
	for _, c := range b {
		h ^= uint64(c)
		h *= prime64
	}
	return strings.ToLower(fmt.Sprintf("%016x", h))
}
