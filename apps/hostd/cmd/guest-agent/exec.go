package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"strconv"
	"sync"
	"syscall"
	"time"
)

// defaultGuestUser is the unprivileged account baked into the golden rootfs at
// uid 1000. Commands run as this user unless one is explicitly requested.
const defaultGuestUser = "user"

const defaultExecTimeout = 30 * time.Second

type execRequest struct {
	Cmd       string            `json:"cmd"`
	Cwd       string            `json:"cwd,omitempty"`
	Env       map[string]string `json:"env,omitempty"`
	User      string            `json:"user,omitempty"`
	TimeoutMS int               `json:"timeout_ms,omitempty"`
}

type execResponse struct {
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	ExitCode int    `json:"exit_code"`
}

func handleExec(w http.ResponseWriter, r *http.Request) {
	var req execRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad request body"})
		return
	}
	if req.Cmd == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "cmd is required"})
		return
	}

	timeout := defaultExecTimeout
	if req.TimeoutMS > 0 {
		timeout = time.Duration(req.TimeoutMS) * time.Millisecond
	}
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "/bin/bash", "-c", req.Cmd)
	if err := prepareCommand(cmd, req.User, req.Cwd, req.Env); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr

	err := cmd.Run()
	resp := execResponse{Stdout: stdout.String(), Stderr: stderr.String()}
	resp.ExitCode = exitCodeOf(err)

	writeJSON(w, http.StatusOK, resp)
}

// prepareCommand applies the user credential first, then the caller's cwd and
// env on top, so an explicit cwd or env always wins over the account defaults.
func prepareCommand(cmd *exec.Cmd, username, cwd string, env map[string]string) error {
	if username == "" {
		username = defaultGuestUser
	}
	if err := applyUserCredential(cmd, username); err != nil {
		return err
	}
	if cwd != "" {
		cmd.Dir = cwd
	}
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	return nil
}

// applyUserCredential runs the command as username.
//
// This FAILS CLOSED if the account does not exist. The predecessor logged a
// warning and continued as root, which silently turns an unprivileged exec
// into a privileged one -- unacceptable when the caller is untrusted code and
// the whole point of the uid is isolation. The golden rootfs always has this
// account; if it does not, the image is wrong and should be fixed, not
// silently escalated around.
func applyUserCredential(cmd *exec.Cmd, username string) error {
	u, err := user.Lookup(username)
	if err != nil {
		return fmt.Errorf("user %q does not exist in this machine", username)
	}
	uid, err := strconv.Atoi(u.Uid)
	if err != nil {
		return fmt.Errorf("user %q has a non-numeric uid %q", username, u.Uid)
	}
	gid, err := strconv.Atoi(u.Gid)
	if err != nil {
		return fmt.Errorf("user %q has a non-numeric gid %q", username, u.Gid)
	}

	cmd.Dir = u.HomeDir
	cmd.Env = append([]string{
		"HOME=" + u.HomeDir,
		"USER=" + u.Username,
		"LOGNAME=" + u.Username,
		"PATH=" + u.HomeDir + "/.local/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"TERM=xterm-256color",
	}, cmd.Env...)

	// Only switch identity if it is actually a switch. Asking the kernel to
	// setuid to the uid we already run as is a no-op that can only fail, and
	// it would make the agent unusable anywhere it is not already root.
	if uid != os.Getuid() || gid != os.Getgid() {
		if cmd.SysProcAttr == nil {
			cmd.SysProcAttr = &syscall.SysProcAttr{}
		}
		cmd.SysProcAttr.Credential = &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid)}
	}
	return nil
}

func exitCodeOf(err error) int {
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	// Could not start, or the context expired: 127 matches a shell's
	// "command not found" convention and is distinguishable from any real
	// exit code the command itself would produce.
	return 127
}

// Frame prefixes for the streaming exec protocol. Byte 0 of every binary frame
// says what the rest is; for an exit frame the payload's first byte is the
// exit code. This is byte-compatible with the sprites protocol so existing
// clients work unchanged.
const (
	frameStdout byte = 1
	frameStderr byte = 2
	frameExit   byte = 3
)

// frameWriter serialises websocket writes.
//
// coder/websocket forbids concurrent writers, and this connection is written
// by three producers: the stdout pump, the stderr pump, and the exit frame.
type frameWriter struct {
	mu   sync.Mutex
	conn wsConn
	ctx  context.Context
}

func (fw *frameWriter) write(kind byte, payload []byte) error {
	fw.mu.Lock()
	defer fw.mu.Unlock()
	return fw.conn.Write(fw.ctx, msgBinary, append([]byte{kind}, payload...))
}

// pump copies one stream into frames until EOF.
func pump(fw *frameWriter, kind byte, r io.Reader, done func()) {
	defer done()
	buf := make([]byte, 32*1024)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			if werr := fw.write(kind, buf[:n]); werr != nil {
				return
			}
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				log.Printf("guest-agent: exec stream read: %v", err)
			}
			return
		}
	}
}
