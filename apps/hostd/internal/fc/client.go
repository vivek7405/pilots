// Package fc drives Firecracker: boot, snapshot, restore, and the process
// bookkeeping that keeps machines alive across a hostd restart.
package fc

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

// apiTimeout bounds a single Firecracker API call. Snapshot creation on a
// large machine is the slowest of them.
const apiTimeout = 60 * time.Second

// Client speaks Firecracker's HTTP API over its unix socket.
type Client struct {
	socketPath string
}

func NewClient(socketPath string) *Client { return &Client{socketPath: socketPath} }

// do issues one API call.
//
// A FRESH transport per call, with keep-alives disabled. Firecracker's
// embedded HTTP server caps total connections at ten, and a pooled transport
// holds idle connections open after each request. On a long-lived hostd those
// leak until every subsequent call to that machine fails with "too many open
// connections" -- and because the cap is per-VM, the failure looks like one
// machine going mad rather than a client bug.
func (c *Client) do(ctx context.Context, method, path string, body any) ([]byte, error) {
	var payload io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("fc: marshal %s %s: %w", method, path, err)
		}
		payload = bytes.NewReader(raw)
	}

	req, err := http.NewRequestWithContext(ctx, method, "http://localhost"+path, payload)
	if err != nil {
		return nil, fmt.Errorf("fc: build %s %s: %w", method, path, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	client := &http.Client{
		Timeout: apiTimeout,
		Transport: &http.Transport{
			DisableKeepAlives: true,
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", c.socketPath)
			},
		},
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fc: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	out, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("fc: read %s %s: %w", method, path, err)
	}
	if resp.StatusCode >= 300 {
		return out, fmt.Errorf("fc: %s %s: status %d: %s", method, path, resp.StatusCode, bytes.TrimSpace(out))
	}
	return out, nil
}

// HugePages2M backs guest memory with 2MiB hugetlbfs pages. Firecracker's
// enum is "None" (its default) or "2M"; pilots sends the field only when it
// wants the latter, so the wire stays byte-identical for a 4KiB machine.
//
// Three constraints come with it, all of them fleet-wide rather than
// per-machine:
//
//   - The page size is recorded IN the snapshot and cannot be changed at
//     restore. Firecracker reads it back out of the saved vm_info, and its own
//     docs say there is no option to flip between 4KiB and huge pages at
//     restore time. So a 2MiB snapshot cannot run on a host without a
//     hugepage pool, and a 4KiB template cannot be restored by a host
//     configured for 2MiB. A half-migrated fleet cannot restore its own
//     machines.
//   - A hugepage snapshot restores ONLY through the Uffd memory backend.
//     Firecracker refuses the File backend outright for one. pilots always
//     restores through Uffd, so this costs nothing -- but it makes that a
//     requirement rather than a choice.
//   - mem_size_mib must be even. Firecracker rejects an odd MiB count under
//     2MiB backing, and its error names neither the field nor the reason.
const HugePages2M = "2M"

// MachineConfig is the VM's shape. SMT stays off: hyperthread siblings shared
// between tenants are a side-channel, and the density gain is not worth it.
type MachineConfig struct {
	VCPUCount   int    `json:"vcpu_count"`
	MemSizeMiB  int    `json:"mem_size_mib"`
	SMT         bool   `json:"smt"`
	CPUTemplate string `json:"cpu_template,omitempty"`
	// HugePages is HugePages2M or empty. See HugePages2M for why this is a
	// property of the whole fleet.
	HugePages string `json:"huge_pages,omitempty"`
}

func (c *Client) SetMachineConfig(ctx context.Context, cfg MachineConfig) error {
	_, err := c.do(ctx, http.MethodPut, "/machine-config", cfg)
	return err
}

type BootSource struct {
	KernelImagePath string `json:"kernel_image_path"`
	BootArgs        string `json:"boot_args"`
}

func (c *Client) SetBootSource(ctx context.Context, src BootSource) error {
	_, err := c.do(ctx, http.MethodPut, "/boot-source", src)
	return err
}

type Drive struct {
	DriveID      string `json:"drive_id"`
	PathOnHost   string `json:"path_on_host"`
	IsRootDevice bool   `json:"is_root_device"`
	IsReadOnly   bool   `json:"is_read_only"`
	// CacheType is "Unsafe" (Firecracker's default) or "Writeback". Omitted
	// leaves the default, which does not advertise the VirtIO flush feature --
	// so the guest's fsync is a no-op. Every drive holding data a user expects
	// to survive must set CacheTypeWriteback; see the constant.
	CacheType string `json:"cache_type,omitempty"`
}

func (c *Client) SetDrive(ctx context.Context, d Drive) error {
	_, err := c.do(ctx, http.MethodPut, "/drives/"+d.DriveID, d)
	return err
}

// VMConfig is the configuration Firecracker reports it is actually running.
type VMConfig struct {
	Drives []Drive `json:"drives"`
}

// GetVMConfig reads the machine's live configuration back out of Firecracker.
//
// Asking the VMM rather than reporting what hostd meant to configure is the
// entire point. A volume drive's cache type is the difference between a
// durable fsync and a lie, it is set in one place and baked into every
// snapshot after that, and the failure mode is silent -- so the value that
// gets checked has to be the one Firecracker holds, not the one we intended.
func (c *Client) GetVMConfig(ctx context.Context) (*VMConfig, error) {
	raw, err := c.do(ctx, http.MethodGet, "/vm/config", nil)
	if err != nil {
		return nil, err
	}
	var cfg VMConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("fc: decode vm config: %w", err)
	}
	return &cfg, nil
}

// DriveConfig returns the live configuration of one drive.
func (c *Client) DriveConfig(ctx context.Context, driveID string) (*Drive, error) {
	cfg, err := c.GetVMConfig(ctx)
	if err != nil {
		return nil, err
	}
	for i := range cfg.Drives {
		if cfg.Drives[i].DriveID == driveID {
			return &cfg.Drives[i], nil
		}
	}
	return nil, fmt.Errorf("fc: no drive %q is attached", driveID)
}

type NetworkInterface struct {
	IfaceID     string `json:"iface_id"`
	HostDevName string `json:"host_dev_name"`
	GuestMAC    string `json:"guest_mac"`
}

func (c *Client) SetNetworkInterface(ctx context.Context, ni NetworkInterface) error {
	_, err := c.do(ctx, http.MethodPut, "/network-interfaces/"+ni.IfaceID, ni)
	return err
}

func (c *Client) Start(ctx context.Context) error {
	_, err := c.do(ctx, http.MethodPut, "/actions", map[string]string{"action_type": "InstanceStart"})
	return err
}

// Pause and Resume drive the VM state machine. Both are idempotent here: an
// InvalidStateTransition into the state we already wanted is success, which
// matters because suspend and checkpoint can race the idle monitor.
func (c *Client) Pause(ctx context.Context) error {
	return c.setVMState(ctx, "Paused")
}

func (c *Client) Resume(ctx context.Context) error {
	return c.setVMState(ctx, "Resumed")
}

func (c *Client) setVMState(ctx context.Context, state string) error {
	_, err := c.do(ctx, http.MethodPatch, "/vm", map[string]string{"state": state})
	if err != nil && isAlreadyInState(err) {
		return nil
	}
	return err
}

func isAlreadyInState(err error) bool {
	return err != nil && bytes.Contains([]byte(err.Error()), []byte("InvalidStateTransition"))
}

type SnapshotCreate struct {
	SnapshotType string `json:"snapshot_type"` // "Full"
	SnapshotPath string `json:"snapshot_path"`
	MemFilePath  string `json:"mem_file_path"`
}

func (c *Client) CreateSnapshot(ctx context.Context, s SnapshotCreate) error {
	_, err := c.do(ctx, http.MethodPut, "/snapshot/create", s)
	return err
}

// MemBackend selects how guest memory is supplied on restore. Phase 2 uses
// File, which reads the whole image up front. Phase 3 switches to Uffd for
// lazy paging without changing anything above this layer.
type MemBackend struct {
	BackendType string `json:"backend_type"` // "File" | "Uffd"
	BackendPath string `json:"backend_path"`
}

type SnapshotLoad struct {
	SnapshotPath        string     `json:"snapshot_path"`
	MemBackend          MemBackend `json:"mem_backend"`
	ResumeVM            bool       `json:"resume_vm"`
	EnableDiffSnapshots bool       `json:"enable_diff_snapshots"`
}

func (c *Client) LoadSnapshot(ctx context.Context, s SnapshotLoad) error {
	_, err := c.do(ctx, http.MethodPut, "/snapshot/load", s)
	return err
}

// WaitForSocket blocks until Firecracker's API socket accepts a connection.
func (c *Client) WaitForSocket(ctx context.Context, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		conn, err := net.DialTimeout("unix", c.socketPath, 200*time.Millisecond)
		if err == nil {
			conn.Close()
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("fc: api socket %s not ready after %s: %w", c.socketPath, timeout, err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
}
