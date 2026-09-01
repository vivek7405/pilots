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
func (b *Builder) solveArgs(contextDir, out, cacheName string) []string {
	args := []string{
		"--addr", b.opts.BuildkitSock,
		"build",
		"--frontend", "dockerfile.v0",
		"--local", "context=" + contextDir,
		"--local", "dockerfile=" + contextDir,
		"--output", "type=tar,dest=" + out,
		"--progress", "rawjson",
	}

	// The cache is what makes a redeploy cheap, and it lives in the same
	// object store as everything else so any host can warm any build. Skipped
	// entirely when there is no bucket: a cache export to nowhere fails the
	// build rather than being slower.
	if b.opts.CacheBucket != "" && cacheName != "" {
		spec := fmt.Sprintf("bucket=%s,endpoint_url=%s,region=%s,name=%s",
			b.opts.CacheBucket, b.opts.CacheEndpoint, b.opts.CacheRegion, cacheName)
		args = append(args,
			// mode=max caches intermediate layers too, not just the result.
			"--export-cache", "type=s3,"+spec+",mode=max",
			"--import-cache", "type=s3,"+spec)
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
func (b *Builder) solve(ctx context.Context, contextDir, out string,
	record func(api.BuildLogLine)) error {

	args := b.solveArgs(contextDir, out, cacheNameFor(contextDir))
	cmd := exec.CommandContext(ctx, b.opts.BuildctlBin, args...)
	// Its own process group, so a timeout kills the whole build tree rather
	// than leaving buildctl's children running against a daemon that has
	// stopped being watched.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Env = append(os.Environ(),
		"AWS_ACCESS_KEY_ID="+os.Getenv("PILOT_S3_ACCESS_KEY"),
		"AWS_SECRET_ACCESS_KEY="+os.Getenv("PILOT_S3_SECRET_KEY"))

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
