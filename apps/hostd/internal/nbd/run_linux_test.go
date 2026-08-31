package nbd

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/RoaringBitmap/roaring/v2"
	"github.com/google/uuid"
	"golang.org/x/sys/unix"

	"github.com/vivek7405/pilots/hostd/internal/block"
	"github.com/vivek7405/pilots/hostd/internal/procname"
)

// TestMain lets the test binary re-exec itself as a handler.
//
// Start() runs os.Executable(), which under `go test` is this binary. Without
// this dispatch the spawn path -- the ready pipe, the argv, the process group,
// the teardown order -- could only ever be tested by hand.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == SubcommandName {
		// Mirrors cmd/hostd: the rename has to be the first thing the process
		// does, on the main thread, or it lands on the wrong one and the
		// handler still answers to the daemon's name.
		if err := procname.Set(ProcessName); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		if err := runHandlerForTest(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// runHandlerForTest mirrors cmd/hostd's subcommand, minus object storage.
func runHandlerForTest(args []string) error {
	cfg := Config{ReadyFD: 3}
	for i := 0; i < len(args); i++ {
		if args[i] == "--read-only" {
			cfg.ReadOnly = true
			continue
		}
		if i+1 >= len(args) {
			return fmt.Errorf("flag %s wants a value", args[i])
		}
		value := args[i+1]
		i++
		switch args[i-1] {
		case "--device":
			cfg.Device = value
		case "--index":
			if _, err := fmt.Sscanf(value, "%d", &cfg.Index); err != nil {
				return err
			}
		case "--cache":
			cfg.CachePath = value
		case "--control":
			cfg.ControlSock = value
		case "--template-dir":
			cfg.TemplateDir = value
		case "--ready-fd", "--cache-root":
		default:
			return fmt.Errorf("unexpected flag %s", args[i-1])
		}
	}
	return Run(context.Background(), cfg, nil)
}

const devBlock int64 = 4096

func requireNBD(t *testing.T) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("needs root: attaching an nbd device")
	}
	if _, err := os.Stat("/dev/nbd0"); err != nil {
		t.Skip("no /dev/nbd0: modprobe nbd nbds_max=64")
	}
}

// buildTemplate chunkifies a file of distinct blocks into a build directory.
func buildTemplate(t *testing.T, dir string, fills []byte) (string, []byte) {
	t.Helper()

	var raw bytes.Buffer
	for _, f := range fills {
		raw.Write(bytes.Repeat([]byte{f}, int(devBlock)))
	}
	in := filepath.Join(dir, "template.bin")
	if err := os.WriteFile(in, raw.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}

	out := filepath.Join(dir, "template")
	if _, _, err := block.Chunkify(context.Background(), block.ChunkifyOpts{
		In: in, OutDir: out, BuildID: uuid.New(), BlockSize: devBlock,
	}); err != nil {
		t.Fatalf("Chunkify: %v", err)
	}
	return out, raw.Bytes()
}

// The full lazy-disk chain: a template served over a real kernel device, the
// guest's writes landing in a copy-on-write file, and the dirty bitmap coming
// back across the control socket in the shape a checkpoint needs.
func TestHandlerServesATemplateAndCapturesWrites(t *testing.T) {
	requireNBD(t)

	dir := t.TempDir()
	fills := []byte{1, 2, 3, 4, 5, 6, 7, 8}
	templateDir, want := buildTemplate(t, dir, fills)

	cachePath := filepath.Join(dir, "rootfs.cow")
	pool := NewDevicePool(DefaultMaxDevices)

	logFile, err := os.Create(filepath.Join(dir, "handler.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer logFile.Close()

	p, err := Start(context.Background(), pool, StartOptions{
		Config: Config{
			TemplateDir: templateDir,
			CachePath:   cachePath,
			ControlSock: filepath.Join(dir, "nbd.sock"),
		},
		Env:     os.Environ(),
		LogFile: logFile,
	})
	if err != nil {
		t.Fatalf("Start: %v\n%s", err, readLog(t, logFile.Name()))
	}
	defer func() {
		if p != nil {
			_ = p.Stop()
		}
	}()

	// The device must be sized before anything reads it.
	if err := WaitReady(p.Index, 5*time.Second); err != nil {
		t.Fatalf("WaitReady: %v\n%s", err, readLog(t, logFile.Name()))
	}
	if size, _ := DeviceSize(p.Index); size != int64(len(want)) {
		t.Errorf("device is %d bytes, want %d", size, len(want))
	}

	dev, err := os.OpenFile(p.Device, os.O_RDWR|syscall.O_SYNC, 0)
	if err != nil {
		t.Fatalf("open %s: %v", p.Device, err)
	}
	defer dev.Close()

	got := make([]byte, len(want))
	if _, err := dev.ReadAt(got, 0); err != nil {
		t.Fatalf("read device: %v\n%s", err, readLog(t, logFile.Name()))
	}
	if !bytes.Equal(got, want) {
		t.Fatal("the device did not serve the template's bytes")
	}

	// Write one block, the way a guest would.
	mutation := bytes.Repeat([]byte{0x5a}, int(devBlock))
	if _, err := dev.WriteAt(mutation, 3*devBlock); err != nil {
		t.Fatalf("write device: %v", err)
	}
	if err := dev.Sync(); err != nil {
		t.Fatalf("sync device: %v", err)
	}

	readBack := make([]byte, devBlock)
	if _, err := dev.ReadAt(readBack, 3*devBlock); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if !bytes.Equal(readBack, mutation) {
		t.Error("the write did not survive a read through the overlay")
	}

	dirty, err := p.Dirty()
	if err != nil {
		t.Fatalf("Dirty: %v", err)
	}
	if !dirty.Contains(3) {
		t.Errorf("dirty = %v, want block 3 present", dirty.ToArray())
	}
	// Reads must not dirty anything: every clean block that ends up in the
	// bitmap is a block the next checkpoint stores instead of pointing at the
	// template, which is how a diff silently grows to the size of the disk.
	for _, idx := range dirty.ToArray() {
		if idx != 3 {
			t.Errorf("block %d is dirty but was only read", idx)
		}
	}

	dev.Close()
	if err := p.Stop(); err != nil {
		t.Errorf("Stop: %v", err)
	}
	stopped := p
	p = nil

	// The device goes back to the pool and the kernel really let it go.
	if pool.InUse() != 0 {
		t.Errorf("pool still holds %d devices after Stop", pool.InUse())
	}
	if !deviceFree(stopped.Index) {
		t.Errorf("%s is still attached after Stop", stopped.Device)
	}
	if syscall.Kill(stopped.Pid(), 0) == nil {
		t.Error("the handler process is still alive after Stop")
	}

	// The cow file is the machine's disk since its last snapshot. A handler
	// that closed its cache would have deleted it here.
	if _, err := os.Stat(cachePath); err != nil {
		t.Fatalf("the cache file did not survive the handler: %v", err)
	}

	// And it chunkifies into a diff that reproduces exactly what the guest saw.
	assertDiffReproduces(t, dir, cachePath, templateDir, dirty, want, mutation)
}

// assertDiffReproduces closes the loop: chunkify what the handler left behind
// and read the result back through its template, the way a wake does.
func assertDiffReproduces(t *testing.T, dir, cachePath, templateDir string,
	dirty *roaring.Bitmap, want, mutation []byte) {
	t.Helper()
	ctx := context.Background()

	diffDir := filepath.Join(dir, "diff")
	if _, _, err := block.Chunkify(ctx, block.ChunkifyOpts{
		In: cachePath, OutDir: diffDir, BuildID: uuid.New(), BlockSize: devBlock,
		ParentDir: templateDir, Dirty: dirty,
	}); err != nil {
		t.Fatalf("Chunkify(cow): %v", err)
	}

	store := dirStore{}
	parentID := store.add(t, templateDir)
	diffID := store.add(t, diffDir)

	cacheRoot := filepath.Join(dir, "verify-cache")
	parent, err := block.OpenRemoteBuild(ctx, store, parentID, cacheRoot)
	if err != nil {
		t.Fatalf("OpenRemoteBuild(template): %v", err)
	}
	defer parent.Close()

	diff, err := block.OpenRemoteBuild(ctx, store, diffID, cacheRoot)
	if err != nil {
		t.Fatalf("OpenRemoteBuild(diff): %v", err)
	}
	defer diff.Close()
	diff.SetParent(parent)

	expected := append([]byte(nil), want...)
	copy(expected[3*devBlock:4*devBlock], mutation)

	got := make([]byte, 0, len(expected))
	for off := int64(0); off < int64(len(expected)); {
		chunk, err := diff.Slice(ctx, off, int64(len(expected))-off)
		if err != nil {
			t.Fatalf("Slice at %d: %v", off, err)
		}
		got = append(got, chunk...)
		off += int64(len(chunk))
	}

	if !bytes.Equal(got, expected) {
		t.Errorf("the chunkified diff does not reproduce what the guest saw; "+
			"first difference at byte %d", firstDiff(got, expected))
	}
}

// dirStore serves build directories as if they were object storage.
type dirStore map[string][]byte

func (s dirStore) add(t *testing.T, dir string) uuid.UUID {
	t.Helper()

	raw, err := os.ReadFile(filepath.Join(dir, "header"))
	if err != nil {
		t.Fatal(err)
	}
	header, err := block.Deserialize(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "data"))
	if err != nil {
		t.Fatal(err)
	}

	id := header.Metadata.BuildId
	s[id.String()+"/header"] = raw
	s[id.String()+"/data"] = data
	return id
}

func (s dirStore) Get(_ context.Context, key string) ([]byte, error) {
	body, ok := s[key]
	if !ok {
		return nil, fmt.Errorf("dirStore: no such key %s", key)
	}
	return body, nil
}

func (s dirStore) GetRange(_ context.Context, key string, off, length int64) ([]byte, error) {
	body, ok := s[key]
	if !ok {
		return nil, fmt.Errorf("dirStore: no such key %s", key)
	}
	if off >= int64(len(body)) {
		return nil, fmt.Errorf("dirStore: %w", block.ErrRangeNotSatisfiable)
	}
	end := off + length
	if end > int64(len(body)) {
		end = int64(len(body))
	}
	return body[off:end], nil
}

func firstDiff(a, b []byte) int {
	for i := range a {
		if i >= len(b) || a[i] != b[i] {
			return i
		}
	}
	return len(a)
}

// A read-only device must reject writes at the kernel boundary rather than
// accepting them into a cache nothing will ever read.
func TestHandlerServesReadOnly(t *testing.T) {
	requireNBD(t)

	dir := t.TempDir()
	templateDir, want := buildTemplate(t, dir, []byte{1, 2, 3, 4})

	pool := NewDevicePool(DefaultMaxDevices)
	logFile, err := os.Create(filepath.Join(dir, "handler.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer logFile.Close()

	p, err := Start(context.Background(), pool, StartOptions{
		Config: Config{
			TemplateDir: templateDir,
			CachePath:   filepath.Join(dir, "rootfs.cow"),
			ControlSock: filepath.Join(dir, "nbd.sock"),
			ReadOnly:    true,
		},
		Env:     os.Environ(),
		LogFile: logFile,
	})
	if err != nil {
		t.Fatalf("Start: %v\n%s", err, readLog(t, logFile.Name()))
	}
	defer p.Stop()

	if err := WaitReady(p.Index, 5*time.Second); err != nil {
		t.Fatalf("WaitReady: %v", err)
	}

	dev, err := os.OpenFile(p.Device, os.O_RDONLY, 0)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer dev.Close()

	got := make([]byte, len(want))
	if _, err := dev.ReadAt(got, 0); err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Error("the read-only device served the wrong bytes")
	}
}

// A handler whose device is disconnected must exit rather than linger holding
// a cow file and a control socket.
func TestStopIsIdempotent(t *testing.T) {
	requireNBD(t)

	dir := t.TempDir()
	templateDir, _ := buildTemplate(t, dir, []byte{1, 2})

	pool := NewDevicePool(DefaultMaxDevices)
	logFile, err := os.Create(filepath.Join(dir, "handler.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer logFile.Close()

	p, err := Start(context.Background(), pool, StartOptions{
		Config: Config{
			TemplateDir: templateDir,
			CachePath:   filepath.Join(dir, "rootfs.cow"),
			ControlSock: filepath.Join(dir, "nbd.sock"),
		},
		Env:     os.Environ(),
		LogFile: logFile,
	})
	if err != nil {
		t.Fatalf("Start: %v\n%s", err, readLog(t, logFile.Name()))
	}

	if err := p.Stop(); err != nil {
		t.Errorf("first Stop: %v", err)
	}
	// A destroy path that retries, or a reaper racing an explicit stop, must
	// not double-release the device -- that is how one machine's device is
	// handed to another while it is still attached.
	if err := p.Stop(); err != nil {
		t.Errorf("second Stop: %v", err)
	}
	if pool.InUse() != 0 {
		t.Errorf("pool holds %d devices after two Stops", pool.InUse())
	}
}

// Stop must not wait out its grace period on a process that exited at once.
//
// A spawned handler becomes a ZOMBIE after SIGTERM, and kill(pid, 0) on a
// zombie succeeds -- so polling for its absence instead of reaping it spends
// the full grace period on every teardown. It looks like nothing: the tests
// pass, the device is released, the machine is destroyed. It just takes five
// seconds, on a path that runs for every machine on the host.
func TestStopDoesNotWaitOutTheGracePeriod(t *testing.T) {
	requireNBD(t)

	dir := t.TempDir()
	templateDir, _ := buildTemplate(t, dir, []byte{1, 2})

	pool := NewDevicePool(DefaultMaxDevices)
	logFile, err := os.Create(filepath.Join(dir, "handler.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer logFile.Close()

	p, err := Start(context.Background(), pool, StartOptions{
		Config: Config{
			TemplateDir: templateDir,
			CachePath:   filepath.Join(dir, "rootfs.cow"),
			ControlSock: filepath.Join(dir, "nbd.sock"),
		},
		Env:     os.Environ(),
		LogFile: logFile,
	})
	if err != nil {
		t.Fatalf("Start: %v\n%s", err, readLog(t, logFile.Name()))
	}

	start := time.Now()
	if err := p.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if elapsed := time.Since(start); elapsed > stopGrace/2 {
		t.Errorf("Stop took %s; a handler that exits promptly must be reaped, "+
			"not polled for until the %s grace period expires", elapsed, stopGrace)
	}
}

func readLog(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		return "(no handler log)"
	}
	return string(raw)
}

// NBD_DO_IT parks in wait_event_interruptible for the life of the device, and
// any signal delivered to the thread holding it makes that wait return early
// -- at which point the kernel shuts the sockets down and clears the queue,
// and the device is finished. The Go runtime sends SIGURG to preempt
// goroutines, so an unmasked DO_IT dies on the runtime's own scheduling.
//
// The symptom is thoroughly misleading: the attach succeeds and the kernel
// logs a capacity change, then the size drops back to zero, so the caller
// fails on a sizing timeout against a device that reports no owner and looks
// perfectly free. It hit roughly one restore in four.
func TestDeviceSurvivesSignalsToTheHandler(t *testing.T) {
	requireNBD(t)

	dir := t.TempDir()
	fills := []byte{1, 2, 3, 4}
	templateDir, want := buildTemplate(t, dir, fills)

	pool := NewDevicePool(DefaultMaxDevices)
	logFile, err := os.Create(filepath.Join(dir, "handler.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer logFile.Close()

	p, err := Start(context.Background(), pool, StartOptions{
		Config: Config{
			TemplateDir: templateDir,
			CachePath:   filepath.Join(dir, "rootfs.cow"),
			ControlSock: filepath.Join(dir, "nbd.sock"),
		},
		Env:     os.Environ(),
		LogFile: logFile,
	})
	if err != nil {
		t.Fatalf("Start: %v\n%s", err, readLog(t, logFile.Name()))
	}
	defer p.Stop()

	if err := WaitReady(p.Index, 5*time.Second); err != nil {
		t.Fatalf("WaitReady: %v\n%s", err, readLog(t, logFile.Name()))
	}

	// SIGURG is what the runtime itself uses, so this is the real signal
	// rather than a stand-in. It has to be aimed at EVERY thread, though: a
	// process-directed kill(2) is delivered to whichever thread the kernel
	// picks, which is almost never the one parked in the ioctl -- which is
	// exactly why this bug survived so long in ordinary testing.
	for round := 0; round < 40; round++ {
		for _, tid := range threadsOf(t, p.Pid()) {
			_ = unix.Tgkill(p.Pid(), tid, syscall.SIGURG)
		}
		time.Sleep(2 * time.Millisecond)
	}

	size, err := DeviceSize(p.Index)
	if err != nil || size != int64(len(want)) {
		t.Fatalf("device is %d bytes (err %v) after signalling, want %d; "+
			"the kernel tore it down on an interrupted NBD_DO_IT\n%s",
			size, err, len(want), readLog(t, logFile.Name()))
	}

	// And it must still serve, not merely still be sized.
	dev, err := os.Open(p.Device)
	if err != nil {
		t.Fatalf("open %s: %v", p.Device, err)
	}
	defer dev.Close()

	got := make([]byte, len(want))
	if _, err := dev.ReadAt(got, 0); err != nil {
		t.Fatalf("read after signalling: %v\n%s", err, readLog(t, logFile.Name()))
	}
	if !bytes.Equal(got, want) {
		t.Error("the device served the wrong bytes after signalling")
	}
}

// threadsOf lists a process's thread ids.
func threadsOf(t *testing.T, pid int) []int {
	t.Helper()

	entries, err := os.ReadDir(fmt.Sprintf("/proc/%d/task", pid))
	if err != nil {
		return nil
	}
	tids := make([]int, 0, len(entries))
	for _, e := range entries {
		tid, err := strconv.Atoi(e.Name())
		if err == nil {
			tids = append(tids, tid)
		}
	}
	return tids
}

// A handler is hostd re-executed, so without a rename its comm is "hostd" too
// -- and a pkill aimed at the daemon, from an operator or a supervisor, takes
// every machine's disk server with it. The guests then block forever on a
// device whose server is gone, in a wait no signal clears.
func TestHandlerDoesNotAnswerToTheDaemonsName(t *testing.T) {
	requireNBD(t)

	dir := t.TempDir()
	templateDir, _ := buildTemplate(t, dir, []byte{1, 2})

	pool := NewDevicePool(DefaultMaxDevices)
	logFile, err := os.Create(filepath.Join(dir, "handler.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer logFile.Close()

	p, err := Start(context.Background(), pool, StartOptions{
		Config: Config{
			TemplateDir: templateDir,
			CachePath:   filepath.Join(dir, "rootfs.cow"),
			ControlSock: filepath.Join(dir, "nbd.sock"),
		},
		Env:     os.Environ(),
		LogFile: logFile,
	})
	if err != nil {
		t.Fatalf("Start: %v\n%s", err, readLog(t, logFile.Name()))
	}
	defer p.Stop()

	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/comm", p.Pid()))
	if err != nil {
		t.Fatalf("read comm: %v", err)
	}
	got := strings.TrimSpace(string(raw))
	if got != ProcessName {
		t.Errorf("the handler calls itself %q, want %q; a pkill on the daemon's "+
			"name would reach it", got, ProcessName)
	}
}
