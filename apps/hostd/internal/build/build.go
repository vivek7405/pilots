// Package build turns a Dockerfile context into a bootable rootfs build.
//
// It is a hostd route on every host, not a build service: the architecture has
// no tier that a request can require to be alive, and that includes builds.
// The builder is rootless BuildKit under an unprivileged user, and the output
// is an ordinary generation-0 template build in the content-addressed store --
// which means creating a machine from a build is the existing restore path
// with nothing new in it.
//
// Isolation is not optional here and is the reason for most of the limits
// below. A build runs an arbitrary user Dockerfile on a host that is also
// running other tenants' machines.
package build

import (
	"archive/tar"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/vivek7405/pilots/hostd/internal/api"
	"github.com/vivek7405/pilots/hostd/internal/block"
)

// Uploader publishes a finished build to object storage.
type Uploader interface {
	PutFile(ctx context.Context, key, filePath string) error
}

// Options configures the builder.
type Options struct {
	// WorkRoot holds each build's context, tarball and image.
	WorkRoot string
	// BuildDir is the content-addressed build directory the rest of the engine
	// reads template builds from.
	BuildDir string
	// Chunks publishes the produced build.
	Chunks Uploader

	// BuildctlBin and the rootless daemon's socket.
	BuildctlBin  string
	BuildkitSock string

	// AgentBinary is the guest agent injected into every image.
	AgentBinary string

	// Cache import/export, so a redeploy does not rebuild from scratch. Empty
	// bucket disables both.
	CacheBucket   string
	CacheEndpoint string
	CacheRegion   string
	// The cache backend runs inside buildkitd, which has no credentials of its
	// own: it is a rootless daemon running as another user, deliberately
	// unaware of hostd's configuration. Without these it falls back to the
	// default AWS chain, reaches for the EC2 metadata service, and fails the
	// build after a context deadline that names IMDS rather than the cache.
	CacheAccessKey string
	CacheSecretKey string

	// Limits. Every one of these exists because a build is arbitrary user code
	// running beside other tenants' machines.
	//
	// MaxContextBytes bounds the upload, Timeout bounds the wall clock, and
	// the image floor and ceiling bound what the build can produce. Without
	// the ceiling a Dockerfile that writes a terabyte of zeros takes the host
	// down with it.
	MaxContextBytes int64
	Timeout         time.Duration
	ImageFloorMiB   int
	ImageCeilingMiB int

	// Concurrency is how many builds may run at once. One by default, and for
	// the same reason chunkify is capped at one: both map large files, they
	// run on the machine hosts, and a deploy landing during a checkpoint burst
	// otherwise OOMs the box.
	Concurrency int
}

// Builder runs builds on this host.
type Builder struct {
	opts Options
	logs *logStore
	sem  chan struct{}
	run  runner

	// tarballInput records whether mke2fs here can read a tarball directly.
	// Probed once; see ProbeTarballInput.
	tarballInput bool
}

// runner executes an external command, capturing its combined output. A field
// so the packing and probing paths can be tested without BuildKit.
type runner func(ctx context.Context, name string, args ...string) ([]byte, error)

func execRunner(ctx context.Context, name string, args ...string) ([]byte, error) {
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("%s: %w: %s", name, err, strings.TrimSpace(string(out)))
	}
	return out, nil
}

// Defaults chosen to be survivable on a host that is also running machines.
const (
	defaultMaxContextBytes = 512 << 20
	defaultTimeout         = 30 * time.Minute
	defaultImageFloorMiB   = 2048
	defaultImageCeilingMiB = 32768
)

// New returns a Builder, probing the local toolchain once.
func New(ctx context.Context, opts Options) *Builder {
	if opts.BuildctlBin == "" {
		opts.BuildctlBin = "/opt/pilots/bin/buildctl"
	}
	if opts.BuildkitSock == "" {
		// Only correct when the daemon runs as THIS user, which on a real host
		// it deliberately does not: buildkitd runs rootless as `pilot` so that
		// an arbitrary user Dockerfile is not built by root beside other
		// tenants' machines. hostd is root, so deriving the path from its own
		// uid points at /run/user/0 and the dial fails with a bare "no such
		// file or directory" that names nothing about users.
		//
		// So this default exists for a single-user dev box and nothing else;
		// host-bootstrap.sh writes PILOT_BUILDKIT_SOCK on every real host.
		opts.BuildkitSock = fmt.Sprintf("unix:///run/user/%d/buildkit/buildkitd.sock", os.Getuid())
	}
	if opts.AgentBinary == "" {
		opts.AgentBinary = "/opt/pilots/bin/guest-agent"
	}
	if opts.MaxContextBytes == 0 {
		opts.MaxContextBytes = defaultMaxContextBytes
	}
	if opts.Timeout == 0 {
		opts.Timeout = defaultTimeout
	}
	if opts.ImageFloorMiB == 0 {
		opts.ImageFloorMiB = defaultImageFloorMiB
	}
	if opts.ImageCeilingMiB == 0 {
		opts.ImageCeilingMiB = defaultImageCeilingMiB
	}
	if opts.Concurrency <= 0 {
		opts.Concurrency = 1
	}

	b := &Builder{
		opts: opts,
		logs: newLogStore(32),
		sem:  make(chan struct{}, opts.Concurrency),
		run:  execRunner,
	}
	b.tarballInput = ProbeTarballInput(ctx)
	if !b.tarballInput {
		slog.Warn("mke2fs on this host cannot read a tarball, so builds will " +
			"unpack under fakeroot instead. Install an e2fsprogs built with " +
			"libarchive to take the faster path.")
	}
	return b
}

// Result is what a finished build produced.
type Result struct {
	ID string
	// RootfsBuildID is a generation-0 template build. A machine created from
	// it takes the ordinary restore path -- there is deliberately nothing new
	// on the create side of a build.
	RootfsBuildID uuid.UUID
}

// NewID mints a build id.
func NewID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Sprintf("build: entropy unavailable: %v", err))
	}
	return "bld-" + hex.EncodeToString(b)
}

// Log returns a build's log, if this host still holds it.
func (b *Builder) Log(id string) (*Log, bool) { return b.logs.get(id) }

// Build runs one build to completion, streaming structured log lines.
//
// emit is called for every line as it happens AND every line is recorded, so a
// client that attaches late or reconnects gets the same stream. The returned
// error is the build's failure; the failing step has already been emitted by
// then, because an agent reading this to patch its own Dockerfile needs to be
// told which step failed rather than left to find it.
func (b *Builder) Build(ctx context.Context, id string, contextTar io.Reader,
	emit func(api.BuildLogLine)) (Result, error) {

	res := Result{ID: id}
	log := b.logs.create(id)
	defer log.Close()

	record := func(line api.BuildLogLine) {
		log.Append(line)
		if emit != nil {
			emit(line)
		}
	}

	// Bounded, and taken before anything expensive. A build competes for
	// memory with the chunkify worker on a host that is also serving machines.
	select {
	case b.sem <- struct{}{}:
		defer func() { <-b.sem }()
	case <-ctx.Done():
		return res, ctx.Err()
	}

	ctx, cancel := contextWithTimeout(ctx, b.opts.Timeout)
	defer cancel()

	work := filepath.Join(b.opts.WorkRoot, id)
	if err := os.MkdirAll(work, 0o755); err != nil {
		return res, fmt.Errorf("build: work dir: %w", err)
	}
	defer os.RemoveAll(work)

	record(status(id, "receiving context"))
	ctxDir := filepath.Join(work, "context")
	if err := extractContext(contextTar, ctxDir, b.opts.MaxContextBytes); err != nil {
		record(failure("receiving context", err))
		return res, err
	}
	if _, err := os.Stat(filepath.Join(ctxDir, "Dockerfile")); err != nil {
		err := errors.New("the build context has no Dockerfile at its root")
		record(failure("receiving context", err))
		return res, err
	}

	// Read before the build rather than after: it is the caller's Dockerfile
	// either way, and a build that runs for ten minutes and then fails on an
	// unparseable start spec has wasted ten minutes.
	dockerfile, err := os.ReadFile(filepath.Join(ctxDir, "Dockerfile"))
	if err != nil {
		record(failure("receiving context", err))
		return res, err
	}
	start := ParseStartSpec(string(dockerfile))
	if start.Empty() {
		// Not a failure. The Dockerfile may inherit its command from its base
		// image, which the tar exporter cannot show us -- but a deploy that
		// then has nothing to run should be able to see that this was known at
		// build time rather than discovered at boot.
		record(status(id, "this Dockerfile declares no CMD or ENTRYPOINT; the "+
			"start command will have to come from the service spec"))
	}

	tarPath := filepath.Join(work, "rootfs.tar")
	if err := b.solve(ctx, ctxDir, tarPath, record); err != nil {
		return res, err
	}

	record(status(id, "packing rootfs"))
	imagePath := filepath.Join(work, "rootfs.ext4")
	if err := b.buildImage(ctx, tarPath, imagePath, start); err != nil {
		record(failure("packing rootfs", err))
		return res, err
	}

	record(status(id, "publishing"))
	buildID, err := b.publish(ctx, imagePath)
	if err != nil {
		record(failure("publishing", err))
		return res, err
	}
	res.RootfsBuildID = buildID

	record(api.BuildLogLine{
		Step: id, Stream: "status", Line: "build complete",
		Result: buildID.String(), TS: nowMillis(),
	})
	return res, nil
}

// buildImage turns the flattened tarball into a bootable ext4.
func (b *Builder) buildImage(ctx context.Context, tarPath, imagePath string, start StartSpec) error {
	info, err := os.Stat(tarPath)
	if err != nil {
		return fmt.Errorf("build: the build produced no filesystem: %w", err)
	}

	hasSystemd, err := tarHasSystemd(tarPath)
	if err != nil {
		return err
	}
	if err := applyFixups(tarPath, Fixups{
		AgentBinary: b.opts.AgentBinary,
		AgentToken:  GuestAgentPlaceholderToken,
		Start:       start,
	}, hasSystemd); err != nil {
		return err
	}

	size := imageSizeMiB(info.Size(), b.opts.ImageFloorMiB, b.opts.ImageCeilingMiB)
	return b.pack(ctx, tarPath, imagePath, size)
}

// GuestAgentPlaceholderToken is what a built image ships with. Every machine
// replaces it at create time -- a shared credential in an image would let any
// guest speak for any other -- and it is the same placeholder the golden
// rootfs carries so the create path needs no special case.
const GuestAgentPlaceholderToken = "placeholder-replaced-at-create"

// publish chunkifies the image as a generation-0 template build and uploads it.
//
// Generation 0 with BaseBuildId == BuildId is what makes this a TEMPLATE
// rather than a diff, and that is the whole trick: a machine created from it
// restores exactly the way one created from the golden template does, so the
// create path learns nothing about builds.
func (b *Builder) publish(ctx context.Context, imagePath string) (uuid.UUID, error) {
	buildID := uuid.New()
	outDir := filepath.Join(b.opts.BuildDir, buildID.String())

	if _, _, err := block.Chunkify(ctx, block.ChunkifyOpts{
		In: imagePath, OutDir: outDir, BuildID: buildID,
	}); err != nil {
		return uuid.Nil, fmt.Errorf("build: chunkify the image: %w", err)
	}
	if b.opts.Chunks == nil {
		return uuid.Nil, errors.New("build: no chunk store is configured")
	}
	for _, part := range []string{"data", "header"} {
		if err := b.opts.Chunks.PutFile(ctx, buildID.String()+"/"+part,
			filepath.Join(outDir, part)); err != nil {
			return uuid.Nil, fmt.Errorf("build: upload %s: %w", part, err)
		}
	}
	return buildID, nil
}

func status(step, line string) api.BuildLogLine {
	return api.BuildLogLine{Step: step, Stream: "status", Line: line, TS: nowMillis()}
}

// failure is the line an agent reads to decide what to patch. It carries the
// step AND the message, so nothing has to infer which step ended the build.
func failure(step string, err error) api.BuildLogLine {
	return api.BuildLogLine{
		Step: step, Stream: "stderr", Line: err.Error(),
		Error: err.Error(), TS: nowMillis(),
	}
}

func contextWithTimeout(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	if d <= 0 {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, d)
}

// extractContext unpacks the uploaded tar into a directory for BuildKit.
//
// Bounded twice over. The byte budget is what stops a client filling the
// host's disk with a POST, and every entry is checked for path traversal --
// this is an untrusted archive, and `../../etc/ssh/authorized_keys` inside it
// is the oldest trick there is.
//
// An escaping path is REFUSED rather than clamped back inside the root.
// Clamping is safe and silently changes the shape of the caller's context, so
// the build fails later in a way that has nothing to do with the cause.
func extractContext(r io.Reader, dir string, maxBytes int64) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("build: context dir: %w", err)
	}
	root, err := filepath.Abs(dir)
	if err != nil {
		return err
	}

	tr := tar.NewReader(r)
	var written int64

	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("build: read the context archive: %w", err)
		}

		rel := filepath.Clean(h.Name)
		if filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
			return fmt.Errorf("build: the context archive contains a path outside "+
				"itself: %q", h.Name)
		}
		target := filepath.Join(root, rel)

		// The size is checked from the HEADER, before a byte is written. A
		// LimitReader around the whole archive would trip in the middle of an
		// entry and surface as "unexpected EOF" -- a corrupt-archive message
		// for a caller whose only mistake was sending too much.
		if h.Typeflag == tar.TypeReg {
			written += h.Size
			if written > maxBytes {
				return fmt.Errorf("build: the build context is larger than the %d "+
					"byte limit", maxBytes)
			}
		}

		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return fmt.Errorf("build: mkdir %s: %w", target, err)
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return fmt.Errorf("build: mkdir %s: %w", filepath.Dir(target), err)
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, os.FileMode(h.Mode)&0o777)
			if err != nil {
				return fmt.Errorf("build: create %s: %w", target, err)
			}
			// Still bounded at the copy: the header's size is the archive's
			// claim about itself, and an archive that lies about it would
			// otherwise write as much as it liked.
			_, err = io.Copy(f, io.LimitReader(tr, h.Size))
			f.Close()
			if err != nil {
				return fmt.Errorf("build: write %s: %w", target, err)
			}
		case tar.TypeSymlink:
			// Skipped rather than recreated. A symlink in an untrusted archive
			// is how a later entry in the same archive writes outside the
			// context, and a Dockerfile has no legitimate need of one.
			continue
		default:
			continue
		}
	}
}
