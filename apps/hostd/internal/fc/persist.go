package fc

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/vivek7405/pilots/hostd/internal/nbd"
	"github.com/vivek7405/pilots/hostd/internal/uffd"
)

// Breadcrumb file names. The pid is kept separate from the JSON deliberately:
// a liveness check is the most common read, and it should not depend on being
// able to parse a state file that may have been truncated.
const (
	pidFile   = "fc.pid"
	stateFile = "state.json"
)

// State is what hostd needs to re-adopt a machine after a restart.
//
// This lives on persistent disk, not /var/run. The predecessor kept it on
// tmpfs, which survives a daemon restart but not a host reboot -- so a reboot
// silently orphaned every machine's bookkeeping.
type State struct {
	MachineID   string `json:"machine_id"`
	Pid         int    `json:"pid"`
	SlotIdx     int    `json:"slot_idx"`
	MAC         string `json:"mac"`
	ChrootDir   string `json:"chroot_dir"`
	SerialLog   string `json:"serial_log"`
	SocketPath  string `json:"socket_path"`
	NetnsName   string `json:"netns_name"`
	StartedAtNs int64  `json:"started_at_unix_ns"`

	// The block and fault servers, so a restarted hostd can pick them back up.
	//
	// Without these a restart adopts the machine but not its handlers: the
	// device stays attached and the processes keep running with nothing
	// holding a handle to them, so destroying the machine leaks both -- and
	// that device is unusable until the host reboots.
	NBDPid      int    `json:"nbd_pid,omitempty"`
	NBDIndex    int    `json:"nbd_index"`
	NBDControl  string `json:"nbd_control,omitempty"`
	UffdPid     int    `json:"uffd_pid,omitempty"`
	UffdSocket  string `json:"uffd_socket,omitempty"`
	UffdControl string `json:"uffd_control,omitempty"`
}

// Persist writes the breadcrumbs for a running machine.
//
// Called after every successful boot and restore. A failure here is not fatal
// to the machine -- it is already running and serving -- but it does mean a
// restart will not re-adopt it, so the caller should log it loudly.
func (m *Machine) Persist() error {
	st := State{
		MachineID:   m.ID,
		ChrootDir:   m.ChrootDir,
		SerialLog:   m.SerialLog,
		SocketPath:  filepath.Join(m.ChrootDir, "run", "fc.sock"),
		StartedAtNs: m.StartedAt.UnixNano(),
	}
	// An adopted machine carries no slot: its namespace already exists and was
	// recorded by whichever process created it.
	if m.Slot != nil {
		st.SlotIdx = m.Slot.Idx
		st.NetnsName = m.Slot.NetnsName
	}
	if m.Cmd != nil && m.Cmd.Process != nil {
		st.Pid = m.Cmd.Process.Pid
	}
	if m.NBD != nil {
		st.NBDPid = m.NBD.Pid()
		st.NBDIndex = m.NBD.Index
		st.NBDControl = nbd.ControlSockFor(m.StateDir)
	}
	if m.Uffd != nil {
		st.UffdPid = m.Uffd.Pid()
		st.UffdSocket = m.Uffd.Socket
		st.UffdControl = uffd.ControlSockFor(m.StateDir)
	}
	return WriteState(m.StateDir, st)
}

// WriteState persists a machine's state atomically.
func WriteState(dir string, st State) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("fc: mkdir %s: %w", dir, err)
	}

	raw, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return fmt.Errorf("fc: marshal state: %w", err)
	}

	// Write to a temp file and rename. A SIGKILL partway through a direct
	// write leaves a truncated file that fails to parse, and then every
	// subsequent reconcile skips the machine -- turning one crash into a
	// permanently orphaned VM.
	tmp := filepath.Join(dir, stateFile+".tmp")
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return fmt.Errorf("fc: write state: %w", err)
	}
	if err := os.Rename(tmp, filepath.Join(dir, stateFile)); err != nil {
		return fmt.Errorf("fc: rename state: %w", err)
	}

	if err := os.WriteFile(filepath.Join(dir, pidFile),
		[]byte(strconv.Itoa(st.Pid)+"\n"), 0o644); err != nil {
		return fmt.Errorf("fc: write pid: %w", err)
	}
	return nil
}

// ReadState loads a machine's breadcrumbs.
func ReadState(dir string) (State, error) {
	var st State
	raw, err := os.ReadFile(filepath.Join(dir, stateFile))
	if err != nil {
		return st, err
	}
	if err := json.Unmarshal(raw, &st); err != nil {
		return st, fmt.Errorf("fc: parse state in %s: %w", dir, err)
	}
	return st, nil
}

// ReadPid reads just the pid, without parsing the state file.
func ReadPid(dir string) (int, error) {
	raw, err := os.ReadFile(filepath.Join(dir, pidFile))
	if err != nil {
		return 0, err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		return 0, fmt.Errorf("fc: parse pid in %s: %w", dir, err)
	}
	return pid, nil
}

// ClearBreadcrumbs removes the evidence that a machine was running.
//
// While fc.pid exists, reconcile believes the machine is live. Failing to
// remove it means the next hostd start tries to re-adopt a machine that was
// deliberately destroyed.
func ClearBreadcrumbs(dir string) error {
	if dir == "" {
		return nil
	}
	for _, f := range []string{pidFile, stateFile, stateFile + ".tmp"} {
		if err := os.Remove(filepath.Join(dir, f)); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("fc: remove %s: %w", f, err)
		}
	}
	return nil
}

// Reconciled is a machine re-adopted after a hostd restart.
type Reconciled struct {
	State State
	Alive bool
}

// Reconcile scans the machine state root and reports what is still running.
//
// This is what makes hostd restartable without disturbing its machines:
// SIGTERM detaches rather than tearing down, and the next start re-adopts
// whatever it finds alive.
//
// A reconciled process is NOT our child, so exec.Cmd.Wait would fail on it --
// but Signal and Kill work, which is all that teardown needs.
func Reconcile(root string) ([]Reconciled, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("fc: read %s: %w", root, err)
	}

	var out []Reconciled
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(root, e.Name())

		st, err := ReadState(dir)
		if err != nil {
			// No state, or unreadable: fall back to the pid file alone, which
			// is enough to decide whether something needs killing.
			if pid, perr := ReadPid(dir); perr == nil && LiveProcess(pid) != 0 {
				out = append(out, Reconciled{
					State: State{MachineID: e.Name(), Pid: pid}, Alive: true,
				})
			}
			continue
		}
		out = append(out, Reconciled{State: st, Alive: LiveProcess(st.Pid) != 0})
	}
	return out, nil
}

// Age reports how long ago the machine started, used by the reaper's age guard.
func (s State) Age() time.Duration {
	if s.StartedAtNs == 0 {
		return 0
	}
	return time.Since(time.Unix(0, s.StartedAtNs))
}

// Adopted rebuilds a Machine handle for a process that outlived hostd.
//
// The process is not our child, so exec.Cmd.Wait would fail on it -- but
// Signal and Kill work, which is everything teardown needs. Wrapping the pid
// in a Process here lets an adopted machine flow through exactly the same code
// paths as one this process started.
func Adopted(st State, stateRoot string, pool *nbd.DevicePool) *Machine {
	proc, err := os.FindProcess(st.Pid)
	if err != nil {
		return nil
	}
	m := &Machine{
		ID:        st.MachineID,
		Cmd:       &exec.Cmd{Process: proc},
		Client:    NewClient(st.SocketPath),
		ChrootDir: st.ChrootDir,
		StateDir:  filepath.Join(stateRoot, st.MachineID),
		SerialLog: st.SerialLog,
		StartedAt: time.Unix(0, st.StartedAtNs),
	}

	// Re-attach the handlers. They outlived this daemon by design -- that is
	// the whole reason they are separate processes -- so they are picked back
	// up rather than restarted, which would tear the disk out from under a
	// running guest.
	//
	// Reserving the device matters as much as the handle: an unreserved index
	// whose handler is still attached would be scanned as busy today and
	// handed out the moment that handler exits, while this machine still
	// believes it owns it.
	if st.NBDPid > 0 && pool != nil {
		m.NBD = nbd.AdoptedProcess(pool, st.NBDPid, st.NBDIndex, st.NBDControl)
	}
	if st.UffdPid > 0 {
		m.Uffd = uffd.AdoptedProcess(st.UffdPid, st.UffdSocket, st.UffdControl)
	}
	return m
}
