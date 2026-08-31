package fc

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/vivek7405/pilots/hostd/internal/netns"
)

// initRetryInterval and initDeadline bound the clock-correction poke.
//
// Aggressive on purpose: until it lands the guest's wall clock is stuck at the
// instant of the snapshot, and the failure mode is silent -- the guest accepts
// TCP connections and never services them, and TLS fails on certificate
// validity windows.
const (
	initRetryInterval = 5 * time.Millisecond
	initDeadline      = 15 * time.Second
)

func jailerArgs(cfg Config) []string {
	args := []string{
		"--id", cfg.MachineID,
		"--exec-file", cfg.FirecrackerBin,
		"--uid", strconv.Itoa(cfg.JailUID),
		"--gid", strconv.Itoa(cfg.JailGID),
		"--chroot-base-dir", cfg.ChrootBase,
		"--netns", cfg.Slot.NetnsPath(),
		"--cgroup-version", "2",
		"--parent-cgroup", "pilots",
	}
	for _, c := range cgroupArgs(cfg.Limits) {
		args = append(args, "--cgroup", c)
	}
	return append(args, "--", "--api-sock", "/run/fc.sock")
}

// fetchIfAbsent serves an immutable object from cache when it is present.
func fetchIfAbsent(ctx context.Context, dl Uploader, key, dest string) error {
	if _, err := os.Stat(dest); err == nil {
		return nil
	}
	return dl.GetToFile(ctx, key, dest)
}

// fetchAlways re-downloads, for artifact sets whose contents change under a
// stable key.
func fetchAlways(ctx context.Context, dl Uploader, key, dest string) error {
	return dl.GetToFile(ctx, key, dest)
}

// isMissing reports a genuinely absent object, as distinct from a failed
// fetch. Only the former is allowed to fall back to the template.
func isMissing(err error) bool {
	return errors.Is(err, os.ErrNotExist) || errors.Is(err, ErrArtifactMissing)
}

// pokeGuestClock sets the restored guest's wall clock to now.
//
// CLOCK_REALTIME is frozen at the moment the snapshot was taken; kvm-clock
// keeps CLOCK_MONOTONIC honest but nothing corrects the wall clock. Until this
// lands the machine looks healthy and behaves badly.
func pokeGuestClock(slot *netns.Slot, token, machineID string) {
	deadline := time.Now().Add(initDeadline)
	client := &http.Client{Timeout: 2 * time.Second}
	url := "http://" + slot.AgentAddr() + "/init"

	for time.Now().Before(deadline) {
		body := strings.NewReader(
			fmt.Sprintf(`{"timestamp_nanos":%d}`, time.Now().UnixNano()))

		req, err := http.NewRequest(http.MethodPost, url, body)
		if err != nil {
			return
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+token)

		resp, err := client.Do(req)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(initRetryInterval)
	}
	slog.Error("guest clock was never corrected after restore; its wall clock is "+
		"stuck at snapshot time", "machine", machineID)
}
