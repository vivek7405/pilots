package build

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vivek7405/pilots/hostd/internal/netns"
)

// writeTar builds a flattened filesystem tarball of the shape BuildKit's tar
// exporter produces.
func writeTar(t *testing.T, path string, entries []tar.Header, bodies map[string]string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	tw := tar.NewWriter(f)
	for _, h := range entries {
		h := h
		if h.ModTime.IsZero() {
			h.ModTime = time.Now()
		}
		body := bodies[h.Name]
		if h.Typeflag == tar.TypeReg {
			h.Size = int64(len(body))
		}
		if err := tw.WriteHeader(&h); err != nil {
			t.Fatal(err)
		}
		if h.Typeflag == tar.TypeReg {
			if _, err := tw.Write([]byte(body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
}

func tarNames(t *testing.T, path string) map[string]*tar.Header {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	out := map[string]*tar.Header{}
	tr := tar.NewReader(f)
	for {
		h, err := tr.Next()
		if err != nil {
			break
		}
		hh := *h
		out[strings.TrimPrefix(h.Name, "./")] = &hh
	}
	return out
}

func stageAgent(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "guest-agent")
	if err := os.WriteFile(p, []byte("#!/bin/true\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// Appending to a tar means first removing its end-of-archive marker. Skipping
// that is the worst kind of failure: every appended entry sits behind a
// terminator, no reader ever sees it, and the image builds and boots with no
// agent in it.
func TestFixupsAreActuallyReadableAfterAppending(t *testing.T) {
	dir := t.TempDir()
	tarPath := filepath.Join(dir, "rootfs.tar")
	writeTar(t, tarPath, []tar.Header{
		{Name: "app/index.js", Typeflag: tar.TypeReg, Mode: 0o644, Uid: 0, Gid: 0},
	}, map[string]string{"app/index.js": "console.log(1)\n"})

	if err := applyFixups(tarPath, Fixups{
		AgentBinary: stageAgent(t), AgentToken: "placeholder",
	}, false); err != nil {
		t.Fatalf("applyFixups: %v", err)
	}

	got := tarNames(t, tarPath)
	// The original content survives.
	if _, ok := got["app/index.js"]; !ok {
		t.Error("appending the fixups lost the image's own files")
	}
	for _, want := range []string{
		"etc/resolv.conf", "opt/pilot-agent/guest-agent", "etc/pilot-agent/token", "sbin/init",
	} {
		if _, ok := got[want]; !ok {
			t.Errorf("%s is missing; the appended entries are not readable", want)
		}
	}
	if h := got["opt/pilot-agent/guest-agent"]; h != nil && h.Mode&0o111 == 0 {
		t.Errorf("the guest agent is not executable (mode %o)", h.Mode)
	}
	if h := got["etc/pilot-agent/token"]; h != nil && h.Mode != 0o600 {
		t.Errorf("the token file is mode %o, want 0600", h.Mode)
	}
}

// An image with no init system -- which is most real Dockerfiles -- gets the
// agent as /sbin/init. An image with systemd keeps systemd and runs the agent
// as a unit.
func TestInitTargetDependsOnWhetherTheImageHasSystemd(t *testing.T) {
	agent := stageAgent(t)

	withoutSystemd := filepath.Join(t.TempDir(), "a.tar")
	writeTar(t, withoutSystemd, []tar.Header{
		{Name: "bin/node", Typeflag: tar.TypeReg, Mode: 0o755},
	}, map[string]string{"bin/node": "x"})
	if has, err := tarHasSystemd(withoutSystemd); err != nil || has {
		t.Fatalf("tarHasSystemd = %v, %v; want false", has, err)
	}
	if err := applyFixups(withoutSystemd, Fixups{AgentBinary: agent}, false); err != nil {
		t.Fatal(err)
	}
	if got := tarNames(t, withoutSystemd)["sbin/init"]; got == nil ||
		got.Linkname != "/opt/pilot-agent/guest-agent" {
		t.Fatalf("an image with no init got /sbin/init -> %v", got)
	}

	withSystemd := filepath.Join(t.TempDir(), "b.tar")
	writeTar(t, withSystemd, []tar.Header{
		{Name: "lib/systemd/systemd", Typeflag: tar.TypeReg, Mode: 0o755},
	}, map[string]string{"lib/systemd/systemd": "x"})
	if has, err := tarHasSystemd(withSystemd); err != nil || !has {
		t.Fatalf("tarHasSystemd = %v, %v; want true", has, err)
	}
	if err := applyFixups(withSystemd, Fixups{AgentBinary: agent}, true); err != nil {
		t.Fatal(err)
	}
	got := tarNames(t, withSystemd)
	if got["sbin/init"] == nil || got["sbin/init"].Linkname != "/lib/systemd/systemd" {
		t.Fatalf("an image with systemd got /sbin/init -> %v", got["sbin/init"])
	}
	// The unit has to be ENABLED, and there is no systemctl to do it with in
	// a tarball, so the wants symlink is written directly.
	if got["etc/systemd/system/multi-user.target.wants/guest-agent.service"] == nil {
		t.Error("the agent unit was installed but never enabled, so it never starts")
	}
	// wait-online stalls the boot for about two minutes on a link the kernel
	// already configured from the ip= boot argument.
	mask := got["etc/systemd/system/systemd-networkd-wait-online.service"]
	if mask == nil || mask.Linkname != "/dev/null" {
		t.Errorf("systemd-networkd-wait-online is not masked: %v", mask)
	}
}

// /etc/resolv.conf cannot be written from a Dockerfile: Docker and BuildKit
// bind-mount over it during the build, so anything written there is discarded
// on export. It has to be one of these fixups.
func TestResolvConfIsWrittenAfterTheBuild(t *testing.T) {
	dir := t.TempDir()
	tarPath := filepath.Join(dir, "rootfs.tar")
	writeTar(t, tarPath, []tar.Header{
		{Name: "etc/resolv.conf", Typeflag: tar.TypeReg, Mode: 0o644},
	}, map[string]string{"etc/resolv.conf": "nameserver 127.0.0.11\n"})

	if err := applyFixups(tarPath, Fixups{
		AgentBinary: stageAgent(t), Nameservers: []string{"9.9.9.9"},
	}, false); err != nil {
		t.Fatal(err)
	}

	// Last entry wins on extraction, so the fixup has to come after the
	// image's own copy.
	f, err := os.Open(tarPath)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	var last string
	tr := tar.NewReader(f)
	for {
		h, err := tr.Next()
		if err != nil {
			break
		}
		if strings.TrimPrefix(h.Name, "./") == "etc/resolv.conf" {
			var buf bytes.Buffer
			_, _ = buf.ReadFrom(tr)
			last = buf.String()
		}
	}
	if !strings.Contains(last, "9.9.9.9") {
		t.Fatalf("the image's own resolv.conf won: %q", last)
	}
}

func TestTruncateTarTerminatorRejectsSomethingThatIsNotATar(t *testing.T) {
	p := filepath.Join(t.TempDir(), "junk")
	if err := os.WriteFile(p, bytes.Repeat([]byte("x"), 4096), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := truncateTarTerminator(p); err == nil {
		t.Fatal("a file with no end-of-archive marker was accepted")
	}
}

// The end to end of the packing half: a flattened tarball plus the fixups
// becomes an ext4 image whose contents and ownership are what the tar said.
// Ownership is the reason the tarball route exists at all -- unpacking to a
// directory unprivileged loses every uid, gid and setuid bit.
func TestPackProducesAnExt4WithOwnershipIntact(t *testing.T) {
	for _, bin := range []string{"mke2fs", "debugfs"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s is not installed", bin)
		}
	}
	ctx := context.Background()
	if !ProbeTarballInput(ctx) {
		t.Skip("mke2fs here has no libarchive support")
	}

	dir := t.TempDir()
	tarPath := filepath.Join(dir, "rootfs.tar")
	writeTar(t, tarPath, []tar.Header{
		{Name: "usr/", Typeflag: tar.TypeDir, Mode: 0o755},
		{Name: "usr/bin/", Typeflag: tar.TypeDir, Mode: 0o755},
		{Name: "usr/bin/sudo", Typeflag: tar.TypeReg, Mode: 0o4755, Uid: 0, Gid: 0},
		{Name: "home/", Typeflag: tar.TypeDir, Mode: 0o755},
		{Name: "home/app.js", Typeflag: tar.TypeReg, Mode: 0o644, Uid: 1000, Gid: 1000},
	}, map[string]string{"usr/bin/sudo": "ELF", "home/app.js": "console.log(1)\n"})

	if err := applyFixups(tarPath, Fixups{
		AgentBinary: stageAgent(t), AgentToken: GuestAgentPlaceholderToken,
	}, false); err != nil {
		t.Fatal(err)
	}

	b := &Builder{opts: Options{}, run: execRunner, tarballInput: true}
	img := filepath.Join(dir, "rootfs.ext4")
	if err := b.pack(ctx, tarPath, img, 32); err != nil {
		t.Fatalf("pack: %v", err)
	}

	out, err := exec.Command("debugfs", "-R", "ls -l /home", img).CombinedOutput()
	if err != nil {
		t.Fatalf("debugfs: %v: %s", err, out)
	}
	if !strings.Contains(string(out), "app.js") {
		t.Fatalf("the image is missing the build's own files:\n%s", out)
	}
	if !strings.Contains(string(out), "1000") {
		t.Errorf("ownership was not carried across from the tar headers:\n%s", out)
	}

	// The setuid bit is the one that fails silently: an image where it is lost
	// boots fine and su stops working.
	sudo, err := exec.Command("debugfs", "-R", "ls -l /usr/bin", img).CombinedOutput()
	if err != nil {
		t.Fatalf("debugfs: %v: %s", err, sudo)
	}
	if !strings.Contains(string(sudo), "104755") {
		t.Errorf("the setuid bit was lost:\n%s", sudo)
	}

	// And the agent is really in there, executable.
	agent, err := exec.Command("debugfs", "-R", "ls -l /opt/pilot-agent", img).CombinedOutput()
	if err != nil {
		t.Fatalf("debugfs: %v: %s", err, agent)
	}
	if !strings.Contains(string(agent), "guest-agent") {
		t.Fatalf("the guest agent is not in the image:\n%s", agent)
	}
}

func TestImageSizeMiB(t *testing.T) {
	const mib = 1024 * 1024

	if got := imageSizeMiB(10*mib, 2048, 32768); got != 2048 {
		t.Errorf("a tiny image got %d MiB, want the floor of 2048", got)
	}
	// Room to write. An image sized exactly to its contents boots into an
	// application that cannot open a log file.
	if got := imageSizeMiB(4000*mib, 2048, 32768); got <= 4000 {
		t.Errorf("a 4000 MiB payload got a %d MiB image, which leaves it nowhere "+
			"to write", got)
	}
	// The ceiling is what stops a Dockerfile that writes a terabyte of zeros
	// from taking the host with it.
	if got := imageSizeMiB(100_000*mib, 2048, 32768); got != 32768 {
		t.Errorf("got %d MiB, want the ceiling of 32768", got)
	}
}

// The bug the read-based terminator lookup exists for.
//
// An entry whose own content ends with 512 zero bytes pads out to a zero
// record that is byte-identical to the end-of-archive marker. A truncation
// that walks back over trailing zeros from the end of the file eats into that
// entry, and the archive is silently cut mid-content -- an image missing
// whatever came last, with nothing to report it.
func TestFixupsSurviveAnEntryThatEndsInZeros(t *testing.T) {
	dir := t.TempDir()
	tarPath := filepath.Join(dir, "rootfs.tar")

	body := make([]byte, 1536) // three whole records, the last two all zeros
	copy(body, []byte("real content"))
	writeTar(t, tarPath, []tar.Header{
		{Name: "data.bin", Typeflag: tar.TypeReg, Mode: 0o644},
	}, map[string]string{"data.bin": string(body)})

	if err := applyFixups(tarPath, Fixups{AgentBinary: stageAgent(t)}, false); err != nil {
		t.Fatalf("applyFixups: %v", err)
	}

	f, err := os.Open(tarPath)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	var seenData, seenAgent bool
	tr := tar.NewReader(f)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("the archive is no longer readable: %v", err)
		}
		switch strings.TrimPrefix(h.Name, "./") {
		case "data.bin":
			got, err := io.ReadAll(tr)
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != len(body) || !bytes.Equal(got, body) {
				t.Fatalf("the entry was truncated: %d bytes back, want %d", len(got), len(body))
			}
			seenData = true
		case "opt/pilot-agent/guest-agent":
			seenAgent = true
		}
	}
	if !seenData {
		t.Error("the original entry disappeared")
	}
	if !seenAgent {
		t.Error("the fixups were not appended")
	}
}

func TestTruncateTarTerminatorRejectsAnEmptyArchive(t *testing.T) {
	p := filepath.Join(t.TempDir(), "empty.tar")
	writeTar(t, p, nil, nil)
	if err := truncateTarTerminator(p); err == nil {
		t.Fatal("an archive with no entries was accepted; there is nothing to build from it")
	}
}

// The start spec has to reach the image, because the agent execs the
// application after env delivery and the tar exporter carries no CMD,
// ENTRYPOINT or WORKDIR of its own.
func TestFixupsCarryTheStartSpecIntoTheImage(t *testing.T) {
	dir := t.TempDir()
	tarPath := filepath.Join(dir, "rootfs.tar")
	writeTar(t, tarPath, []tar.Header{
		{Name: "app/server.js", Typeflag: tar.TypeReg, Mode: 0o644},
	}, map[string]string{"app/server.js": "x"})

	spec := ParseStartSpec("FROM node:24-alpine\nWORKDIR /app\nEXPOSE 3000\nCMD [\"node\",\"server.js\"]\n")
	if err := applyFixups(tarPath, Fixups{
		AgentBinary: stageAgent(t), Start: spec,
	}, false); err != nil {
		t.Fatalf("applyFixups: %v", err)
	}

	f, err := os.Open(tarPath)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	var body []byte
	tr := tar.NewReader(f)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if strings.TrimPrefix(h.Name, "./") == StartSpecPath {
			if body, err = io.ReadAll(tr); err != nil {
				t.Fatal(err)
			}
		}
	}
	if body == nil {
		t.Fatalf("%s is not in the image; nothing tells the agent what to start", StartSpecPath)
	}

	var got StartSpec
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("the start spec in the image is not readable json: %v", err)
	}
	if len(got.Cmd) != 2 || got.Cmd[0] != "node" || got.WorkDir != "/app" || got.Port != 3000 {
		t.Fatalf("the spec did not survive the round trip: %+v", got)
	}
}

// The fallback path, exercised for real rather than assumed.
//
// A host whose e2fsprogs was built without libarchive takes packViaDirectory
// instead, and the property that has to survive is the one the tarball route
// exists to protect: ownership and the setuid bit, produced by an
// unprivileged process. fakeroot is what makes that possible, and it only
// works because the extract and the mke2fs share ONE session -- its uid map
// lives in memory per session.
func TestPackViaDirectoryKeepsOwnershipUnderFakeroot(t *testing.T) {
	for _, bin := range []string{"mke2fs", "debugfs", "fakeroot", "tar"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s is not installed", bin)
		}
	}

	dir := t.TempDir()
	tarPath := filepath.Join(dir, "rootfs.tar")
	writeTar(t, tarPath, []tar.Header{
		{Name: "usr/", Typeflag: tar.TypeDir, Mode: 0o755},
		{Name: "usr/bin/", Typeflag: tar.TypeDir, Mode: 0o755},
		{Name: "usr/bin/sudo", Typeflag: tar.TypeReg, Mode: 0o4755, Uid: 0, Gid: 0},
		{Name: "home/", Typeflag: tar.TypeDir, Mode: 0o755},
		{Name: "home/app.js", Typeflag: tar.TypeReg, Mode: 0o644, Uid: 1000, Gid: 1000},
	}, map[string]string{"usr/bin/sudo": "ELF", "home/app.js": "console.log(1)\n"})

	if err := applyFixups(tarPath, Fixups{
		AgentBinary: stageAgent(t), AgentToken: GuestAgentPlaceholderToken,
	}, false); err != nil {
		t.Fatal(err)
	}

	// tarballInput false is the whole point: this is the path a host without
	// libarchive takes, forced on regardless of what this machine can do.
	b := &Builder{opts: Options{}, run: execRunner, tarballInput: false}
	img := filepath.Join(dir, "rootfs.ext4")
	if err := b.pack(context.Background(), tarPath, img, 32); err != nil {
		t.Fatalf("packViaDirectory: %v", err)
	}

	home, err := exec.Command("debugfs", "-R", "ls -l /home", img).CombinedOutput()
	if err != nil {
		t.Fatalf("debugfs: %v: %s", err, home)
	}
	if !strings.Contains(string(home), "app.js") {
		t.Fatalf("the fallback lost the build's own files:\n%s", home)
	}
	if !strings.Contains(string(home), "1000") {
		t.Errorf("the fallback lost ownership:\n%s", home)
	}

	// The bit that vanishes silently if the extract and the mke2fs are split
	// across two fakeroot sessions.
	sudo, err := exec.Command("debugfs", "-R", "ls -l /usr/bin", img).CombinedOutput()
	if err != nil {
		t.Fatalf("debugfs: %v: %s", err, sudo)
	}
	if !strings.Contains(string(sudo), "104755") {
		t.Errorf("the fallback lost the setuid bit; su would be broken in this "+
			"image and nothing would say so:\n%s", sudo)
	}

	agent, err := exec.Command("debugfs", "-R", "ls -l /opt/pilot-agent", img).CombinedOutput()
	if err != nil {
		t.Fatalf("debugfs: %v: %s", err, agent)
	}
	if !strings.Contains(string(agent), "guest-agent") {
		t.Fatalf("the fallback did not carry the fixups through:\n%s", agent)
	}
}

// The probe itself. It has to answer for THIS host rather than assume, because
// the fallback is a different code path that must be chosen before the build
// starts, not after it has already produced output.
func TestProbeTarballInputAgreesWithWhatPackDoes(t *testing.T) {
	if _, err := exec.LookPath("mke2fs"); err != nil {
		t.Skip("mke2fs is not installed")
	}
	ctx := context.Background()
	supported := ProbeTarballInput(ctx)

	dir := t.TempDir()
	tarPath := filepath.Join(dir, "probe.tar")
	writeTar(t, tarPath, []tar.Header{
		{Name: "file", Typeflag: tar.TypeReg, Mode: 0o644},
	}, map[string]string{"file": "x"})

	b := &Builder{opts: Options{}, run: execRunner, tarballInput: true}
	err := b.pack(ctx, tarPath, filepath.Join(dir, "out.ext4"), 8)
	if supported && err != nil {
		t.Fatalf("the probe said tarball input works and it did not: %v", err)
	}
	if !supported && err == nil {
		t.Fatal("the probe said tarball input does not work and it did; every " +
			"build on this host would take the slow path for no reason")
	}
}

// A built image must resolve through the namespace gateway.
//
// Nameservers was never set by the build path, so every image it produced
// carried public resolvers and could not resolve .internal at all -- and
// build-backed machines are precisely the ones the feature exists for. The
// failure was invisible from outside: the machine booted, answered health
// checks, resolved github.com, and only service discovery was missing.
//
// Both the explicit value and the zero value are checked. The zero value is
// the one that caused this, so it must be safe on its own rather than safe
// because one caller remembers.
func TestABuiltImageResolvesThroughTheGateway(t *testing.T) {
	if got := (Fixups{}).resolvConf(); !strings.Contains(got, netns.TapHostIP) {
		t.Errorf("a Fixups with no nameservers wrote %q; it must name the "+
			"gateway, or the image cannot resolve .internal", got)
	}
	if got := (Fixups{Nameservers: []string{"9.9.9.9"}}).resolvConf(); !strings.Contains(got, "9.9.9.9") {
		t.Errorf("an explicit nameserver was dropped: %q", got)
	}
}
