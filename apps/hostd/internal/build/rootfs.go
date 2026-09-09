package build

import (
	"archive/tar"
	"context"
	"fmt"
	"github.com/vivek7405/pilots/hostd/internal/netns"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"time"
)

// Turning BuildKit's output into a bootable ext4, with no root and no loop
// mount.
//
// The route is: BuildKit's `tar` exporter (the FLATTENED filesystem, not the
// `oci` exporter's layered image tarball) -> append the guest fixups ->
// mke2fs -d. mke2fs reads ownership and mode straight out of the tar headers,
// which is the whole reason the tarball route exists: unpacking to a directory
// as an unprivileged user loses every uid, gid and setuid bit, and produces a
// rootfs where nothing is root-owned and su is broken -- a failure that shows
// up at first boot rather than at build time.

// pack writes a bootable ext4 image from a flattened filesystem tarball.
func (b *Builder) pack(ctx context.Context, tarPath, out string, sizeMiB int) error {
	if b.tarballInput {
		if _, err := b.run(ctx, "mke2fs", "-q", "-F", "-t", "ext4", "-b", "4096",
			"-d", tarPath, out, fmt.Sprintf("%dM", sizeMiB)); err != nil {
			return fmt.Errorf("build: pack %s: %w", out, err)
		}
		return nil
	}
	return b.packViaDirectory(ctx, tarPath, out, sizeMiB)
}

// packViaDirectory is the fallback for an e2fsprogs built without libarchive.
//
// The extract and the mke2fs run inside ONE fakeroot session, and that is not
// tidiness. fakeroot keeps its uid/gid map in memory per session, so splitting
// them across two invocations silently loses every ownership and setuid bit --
// the same broken rootfs the tarball route exists to avoid, arrived at by a
// different road. The golden rootfs script pays for this lesson in a comment
// of its own.
func (b *Builder) packViaDirectory(ctx context.Context, tarPath, out string, sizeMiB int) error {
	root, err := os.MkdirTemp(filepath.Dir(out), "rootfs-")
	if err != nil {
		return fmt.Errorf("build: temp root: %w", err)
	}
	defer os.RemoveAll(root)

	script := fmt.Sprintf(
		`set -eu; tar -xf %q -C %q; mke2fs -q -F -t ext4 -b 4096 -d %q %q %dM`,
		tarPath, root, root, out, sizeMiB)
	if _, err := b.run(ctx, "fakeroot", "sh", "-c", script); err != nil {
		return fmt.Errorf("build: pack %s via a directory: %w", out, err)
	}
	return nil
}

// ProbeTarballInput reports whether mke2fs on this host can read a tarball.
//
// `mke2fs -d <tarball>` needs e2fsprogs compiled with libarchive AND the
// shared library present at run time, and neither is guaranteed. Probed once
// at startup with a real tiny tarball rather than assumed, because a host
// missing it fails every build with an error that names neither libarchive nor
// the fallback -- and the fallback is a different code path that has to be
// chosen before the build starts, not after it has already produced output.
func ProbeTarballInput(ctx context.Context) bool {
	dir, err := os.MkdirTemp("", "pilots-mke2fs-probe-")
	if err != nil {
		return false
	}
	defer os.RemoveAll(dir)

	tarPath := filepath.Join(dir, "probe.tar")
	f, err := os.Create(tarPath)
	if err != nil {
		return false
	}
	tw := tar.NewWriter(f)
	if err := tw.WriteHeader(&tar.Header{
		Name: "probe", Mode: 0o644, Size: 0, ModTime: time.Now(), Typeflag: tar.TypeReg,
	}); err != nil {
		f.Close()
		return false
	}
	if err := tw.Close(); err != nil {
		f.Close()
		return false
	}
	f.Close()

	img := filepath.Join(dir, "probe.ext4")
	cmd := exec.CommandContext(ctx, "mke2fs", "-q", "-F", "-t", "ext4", "-b", "4096",
		"-d", tarPath, img, "2M")
	return cmd.Run() == nil
}

// Fixups is what has to be true of any image before it can boot as a pilots
// machine.
//
// Every one of these is a thing Docker does for a container and the kernel
// does not do for a VM. They are applied to the flattened tarball, after the
// build and before the ext4, because several of them cannot be done inside a
// Dockerfile at all.
type Fixups struct {
	// AgentBinary is the guest agent, installed at /opt/pilot-agent/guest-agent.
	// Without it a machine boots and is unreachable: exec, the clock poke and
	// the port proxy all go through it.
	AgentBinary string
	// Nameservers go into /etc/resolv.conf. It CANNOT be written in the
	// Dockerfile -- Docker and BuildKit bind-mount over it during the build,
	// so anything written there is discarded on export.
	Nameservers []string
	// AgentToken is the placeholder credential the create path replaces. Every
	// machine gets its own; a shared one baked into an image would let any
	// guest speak for any other.
	AgentToken string
	// Start is what the guest agent will exec once env has been delivered. The
	// tar exporter carries no image metadata at all, so this is read out of
	// the Dockerfile instead; see StartSpec for exactly how far that goes.
	Start StartSpec
}

// resolvConf is the file the fixups write.
//
// The default is only a fallback for a caller that names no resolver; every
// real build points at the namespace gateway the way the golden rootfs does,
// because that is where .internal is answered.
func (f Fixups) resolvConf() string {
	ns := f.Nameservers
	if len(ns) == 0 {
		// The gateway, not a public resolver. A caller that names none used
		// to get 8.8.8.8 here, and the image it built could not resolve
		// .internal at all -- which is how build-backed machines, the ones
		// the feature exists for, silently had no service discovery. The
		// responder forwards anything that is not .internal upstream, so
		// this costs the image no public resolution.
		ns = []string{netns.TapHostIP}
	}
	var b strings.Builder
	for _, addr := range ns {
		fmt.Fprintf(&b, "nameserver %s\n", addr)
	}
	return b.String()
}

// AgentPathInImage is where the build installs the guest agent inside an
// image, and therefore what the kernel is told to run as PID 1 when the image
// carries no init of its own. Exported so the boot path names the same path
// this one writes, rather than repeating the string.
const AgentPathInImage = "/opt/pilot-agent/guest-agent"

// guestAgentUnit starts the agent under systemd, for an image that has one.
const guestAgentUnit = `[Unit]
Description=pilots in-VM guest agent
After=network.target

[Service]
ExecStart=/opt/pilot-agent/guest-agent
Environment=AGENT_PORT=3001
Restart=always
RestartSec=1

[Install]
WantedBy=multi-user.target
`

// applyFixups appends the guest fixups to a flattened filesystem tarball.
//
// Appended rather than merged into a directory, so the no-root tarball route
// survives: every entry written here carries the uid, gid and mode it needs,
// and mke2fs honours them.
//
// Later entries win. tar semantics are last-write-wins on extraction and
// mke2fs follows them, so an image that ships its own /etc/resolv.conf or
// /sbin/init is overridden rather than conflicting.
//
// Every appended path is resolved through the image's own directory symlinks
// first -- see imageFacts.resolve. Writing sbin/init verbatim into a
// usr-merged image fails the whole pack.
func applyFixups(tarPath string, f Fixups, img imageFacts) error {
	hasSystemd := img.hasSystemd
	// Truncate the archive's end-of-file marker before appending, or every
	// appended entry sits after a terminator and is simply never read. This
	// fails silently in the worst possible way: the image builds, boots, and
	// has no agent in it.
	if err := truncateTarTerminator(tarPath); err != nil {
		return err
	}

	fh, err := os.OpenFile(tarPath, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("build: open %s to append fixups: %w", tarPath, err)
	}
	defer fh.Close()

	tw := tar.NewWriter(fh)
	defer tw.Close()

	agent, err := os.ReadFile(f.AgentBinary)
	if err != nil {
		return fmt.Errorf("build: read the guest agent at %s: %w", f.AgentBinary, err)
	}

	dirs := []string{"etc/", "sbin/", "opt/", "opt/pilot-agent/", "etc/pilot-agent/"}
	if hasSystemd {
		dirs = append(dirs, "etc/systemd/", "etc/systemd/system/",
			"etc/systemd/system/multi-user.target.wants/")
	}
	for _, d := range dirs {
		if err := tw.WriteHeader(&tar.Header{
			Name: img.resolve(d), Typeflag: tar.TypeDir, Mode: 0o755,
			ModTime: time.Now(),
		}); err != nil {
			return fmt.Errorf("build: write %s: %w", d, err)
		}
	}

	startSpec, err := f.Start.Marshal()
	if err != nil {
		return err
	}

	files := []struct {
		name string
		mode int64
		data []byte
	}{
		{"etc/resolv.conf", 0o644, []byte(f.resolvConf())},
		{"opt/pilot-agent/guest-agent", 0o755, agent},
		{"etc/pilot-agent/token", 0o600, []byte(f.AgentToken)},
		// Not secret and deliberately world-readable: the guest agent reads it
		// as whatever user it ends up running as, and an operator debugging a
		// machine that will not start needs to be able to see it.
		{StartSpecPath, 0o644, startSpec},
		// See scripts/rootfs/Dockerfile: proactive compaction dirties pages
		// the host then has to carry in the next diff and fault back in on
		// the next wake. A user-built image gets the same treatment as the
		// golden one.
		{"etc/sysctl.d/60-pilots-guest.conf", 0o644,
			[]byte("vm.compaction_proactiveness = 0\n")},
	}
	if hasSystemd {
		files = append(files,
			struct {
				name string
				mode int64
				data []byte
			}{"etc/systemd/system/guest-agent.service", 0o644, []byte(guestAgentUnit)})
	}
	// Directories the fixups write into, created first.
	//
	// mke2fs populates from this tar in order and will not create a parent
	// implicitly: a file under a directory the base image happens not to ship
	// fails the whole pack with "cannot find directory ... to create". A slim
	// base image legitimately has no /etc/sysctl.d.
	for _, dir := range []string{"etc/sysctl.d"} {
		if err := tw.WriteHeader(&tar.Header{
			Name: img.resolve(dir + "/"), Typeflag: tar.TypeDir, Mode: 0o755,
			ModTime: time.Now(),
		}); err != nil {
			return fmt.Errorf("build: mkdir %s: %w", dir, err)
		}
	}

	for _, file := range files {
		if err := tw.WriteHeader(&tar.Header{
			Name: img.resolve(file.name), Typeflag: tar.TypeReg, Mode: file.mode,
			Size: int64(len(file.data)), ModTime: time.Now(),
		}); err != nil {
			return fmt.Errorf("build: write %s: %w", file.name, err)
		}
		if _, err := tw.Write(file.data); err != nil {
			return fmt.Errorf("build: write %s: %w", file.name, err)
		}
	}

	links := []struct{ name, target string }{}
	if hasSystemd {
		// The kernel boots /sbin/init; systemd lives elsewhere in the image.
		links = append(links,
			struct{ name, target string }{"sbin/init", "/lib/systemd/systemd"},
			// Enable the agent by writing the wants symlink directly. There is
			// no systemctl to run here -- this is a tarball, not a container.
			struct{ name, target string }{
				"etc/systemd/system/multi-user.target.wants/guest-agent.service",
				"/etc/systemd/system/guest-agent.service"},
			// wait-online stalls the boot for about two minutes on a link the
			// kernel already configured from the ip= boot argument. Masking is
			// a symlink to /dev/null, which is a tar entry like any other.
			struct{ name, target string }{
				"etc/systemd/system/systemd-networkd-wait-online.service", "/dev/null"},
		)
	} else {
		// No init in the image at all, which is the ordinary case for the
		// slim base images real Dockerfiles use. The agent is the init: it
		// mounts the pseudo-filesystems, remounts the root read-write, and
		// reaps orphans. See the guest agent's PID 1 path.
		links = append(links,
			struct{ name, target string }{"sbin/init", AgentPathInImage})
	}
	for _, l := range links {
		if err := tw.WriteHeader(&tar.Header{
			Name: img.resolve(l.name), Typeflag: tar.TypeSymlink,
			Linkname: l.target, Mode: 0o777, ModTime: time.Now(),
		}); err != nil {
			return fmt.Errorf("build: link %s: %w", l.name, err)
		}
	}
	return nil
}

// tarBlock is the archive's fixed record size.
const tarBlock = 512

// truncateTarTerminator removes the end-of-archive marker so entries can be
// appended.
//
// The offset is found by READING the archive rather than by walking back over
// trailing zero blocks from the end. The walk-back version is the obvious one
// and it is wrong: a file whose own last bytes happen to be zeros pads out to
// a zero block that is indistinguishable from the terminator, so the walk eats
// into real content and the archive is silently truncated mid-entry.
func truncateTarTerminator(path string) error {
	f, err := os.OpenFile(path, os.O_RDWR, 0o644)
	if err != nil {
		return fmt.Errorf("build: open %s: %w", path, err)
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return fmt.Errorf("build: stat %s: %w", path, err)
	}

	counter := &countingReader{r: f}
	tr := tar.NewReader(counter)

	var end int64
	var entries int
	for {
		_, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("build: %s is not a readable tar archive: %w", path, err)
		}
		// Consume the entry so the counter is past its data. The padding to
		// the next record boundary is consumed by the following Next(), so it
		// is accounted for by rounding rather than by reading.
		if _, err := io.Copy(io.Discard, tr); err != nil {
			return fmt.Errorf("build: read %s: %w", path, err)
		}
		end = roundUpToBlock(counter.n)
		entries++
	}

	if entries == 0 {
		return fmt.Errorf("build: %s contains no entries; it is not a filesystem "+
			"archive", path)
	}
	if end >= info.Size() {
		return fmt.Errorf("build: %s has no end-of-archive marker; it is not a "+
			"complete tar archive", path)
	}
	return f.Truncate(end)
}

// countingReader reports how many bytes have been consumed, which is how the
// end of the last entry is located exactly.
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

func roundUpToBlock(n int64) int64 {
	if rem := n % tarBlock; rem != 0 {
		return n + tarBlock - rem
	}
	return n
}

// imageFacts is what the fixups need to know about the image they are about
// to be appended to. Both facts are read from the archive rather than guessed
// from the base image name, and both are gathered in one pass.
type imageFacts struct {
	// hasSystemd decides which set of fixups applies. An image with systemd
	// boots it as PID 1 and the agent runs as a unit; an image without one --
	// node:alpine, distroless, anything a real Dockerfile actually uses --
	// gets the agent as PID 1 instead.
	hasSystemd bool

	// dirLinks maps each directory the image reaches only through a symlink
	// to the real directory behind it, e.g. "sbin" -> "usr/sbin" on any
	// usr-merged image. Keys and values are cleaned, root-relative, and carry
	// no trailing slash. Values are fully resolved -- a link to a link is
	// collapsed here, so resolve never has to follow one twice.
	dirLinks map[string]string
}

// resolve rewrites an appended path through the image's directory symlinks.
//
// mke2fs will not create a child inside a symlink. e2fsprogs' libarchive
// reader looks the parent up with ext2fs_namei and stops at the link:
//
//	ext2fs_symlink: Ext2 inode is not a directory while creating symlink "init"
//	mke2fs: Operation not permitted while populating file system
//
// Debian and Ubuntu have been usr-merged for years -- /sbin is a symlink to
// usr/sbin -- so appending sbin/init verbatim fails the pack for most of the
// base images real Dockerfiles are built on, minutes after the image itself
// built cleanly. Writing at usr/sbin/init instead lands the same file at the
// same path, because /sbin/init still resolves through the image's own link.
//
// Only a symlink whose target is a directory in the image is followed. That
// distinction is load-bearing: Ubuntu ships /etc/resolv.conf as a symlink to
// a file under /run, and following THAT would write the fixup into a
// directory the image does not have.
func (img imageFacts) resolve(name string) string {
	if len(img.dirLinks) == 0 {
		return name
	}
	trailing := strings.HasSuffix(name, "/")
	var out string
	for _, part := range strings.Split(strings.TrimSuffix(name, "/"), "/") {
		if out == "" {
			out = part
		} else {
			out += "/" + part
		}
		if target, ok := img.dirLinks[out]; ok {
			out = target
		}
	}
	if trailing {
		out += "/"
	}
	return out
}

// scanImage reads the facts the fixups need out of a flattened image tarball.
//
// One pass for both, because the directory symlinks cannot be classified
// until every directory entry has been seen: a symlink is only followed when
// its target is a directory, and the target may appear anywhere in the
// archive, before or after the link itself.
func scanImage(tarPath string) (imageFacts, error) {
	f, err := os.Open(tarPath)
	if err != nil {
		return imageFacts{}, fmt.Errorf("build: open %s: %w", tarPath, err)
	}
	defer f.Close()

	facts := imageFacts{dirLinks: map[string]string{}}
	dirs := map[string]bool{}
	links := map[string]string{}

	tr := tar.NewReader(f)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return imageFacts{}, fmt.Errorf("build: read %s: %w", tarPath, err)
		}
		// tar names are always slash-separated, whatever the host is.
		name := strings.TrimPrefix(path.Clean("/"+h.Name), "/")
		switch h.Typeflag {
		case tar.TypeDir:
			dirs[name] = true
		case tar.TypeSymlink:
			links[name] = h.Linkname
		}
		if name == "lib/systemd/systemd" || name == "usr/lib/systemd/systemd" {
			facts.hasSystemd = true
		}
	}

	// Classified to a fixed point, because a link may point at another link:
	// with /usr/sbin -> bin and /sbin -> usr/sbin, /sbin is only a directory
	// link once /usr/sbin is known to be one. Map iteration order is random,
	// so a single pass would classify that pair or not depending on the run.
	// Bounded, so a cycle in a hostile image cannot spin here.
	for round := 0; round < 8; round++ {
		grew := false
		for name, target := range links {
			if _, done := facts.dirLinks[name]; done {
				continue
			}
			// An absolute target resolves against the image root; a relative
			// one against the directory the link itself sits in.
			resolved := target
			if !strings.HasPrefix(target, "/") {
				resolved = path.Dir(name) + "/" + target
			}
			resolved = facts.resolve(strings.TrimPrefix(path.Clean("/"+resolved), "/"))
			if dirs[resolved] {
				facts.dirLinks[name] = resolved
				grew = true
			}
		}
		if !grew {
			break
		}
	}
	return facts, nil
}

// imageSizeMiB picks how big the ext4 has to be.
//
// The tarball's size is the payload; the rest is filesystem overhead plus room
// for the machine to write. Generous rather than tight: an image sized exactly
// to its contents boots into an application that cannot write a log file, and
// the image is sparse, so unused space costs nothing until it is used.
func imageSizeMiB(tarBytes int64, floorMiB, ceilingMiB int) int {
	const mib = 1024 * 1024
	size := int(tarBytes/mib)*2 + 512
	if size < floorMiB {
		size = floorMiB
	}
	if ceilingMiB > 0 && size > ceilingMiB {
		size = ceilingMiB
	}
	return size
}
