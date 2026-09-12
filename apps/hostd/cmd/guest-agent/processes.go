package main

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// The guest's process surface: what is running, and how to change it.
//
// A process registered here has to survive a cold boot, or every integration
// that starts one -- the editor mounts, the harness plugins, the MCP tools --
// behaves differently the second time a machine is visited, which reads as the
// integration being broken rather than as a deliberate lifecycle. A warm wake
// needs nothing: the process is IN the snapshot, still running, with its pid
// intact, and re-execing it is exactly the bug startApp's refusal exists to
// prevent. A cold boot has no snapshot to resume, so the declarations are
// replayed from disk instead.
//
// One JSON file per process under appDir, rather than one file holding them
// all: a half-written combined file loses every process, and a machine that
// comes back with no processes at all looks like a platform failure. A
// half-written single file loses one, and the rest still start.

// appDir holds one file per runtime-registered process. A variable so tests
// can write somewhere that is not the real /etc.
var appDir = "/etc/pilot/app.d"

// saveProcess records a runtime registration for the next cold boot.
func saveProcess(spec processSpec) error {
	if err := os.MkdirAll(appDir, 0o700); err != nil {
		return err
	}
	raw, err := json.Marshal(spec)
	if err != nil {
		return err
	}
	// Written to a temporary name and renamed, so a boot that races a
	// registration reads either the old declaration or the new one, never half
	// of one.
	tmp := filepath.Join(appDir, "."+spec.Name+".tmp")
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(appDir, spec.Name+".json"))
}

// forgetProcess removes a registration.
func forgetProcess(name string) error {
	err := os.Remove(filepath.Join(appDir, name+".json"))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// savedProcesses reads back what was registered, in name order.
//
// An unreadable file is skipped rather than fatal: one corrupt declaration
// must not stop the machine's other processes from starting.
func savedProcesses() []processSpec {
	entries, err := os.ReadDir(appDir)
	if err != nil {
		return nil
	}
	var out []processSpec
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(appDir, e.Name()))
		if err != nil {
			continue
		}
		var spec processSpec
		if err := json.Unmarshal(raw, &spec); err != nil || spec.Name == "" || spec.Cmd == "" {
			continue
		}
		out = append(out, spec)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// handleProcesses lists what this machine runs.
func handleProcesses(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"processes": appSupervisor.list()})
}

// handleRegisterProcess adds a process at runtime and starts it.
func handleRegisterProcess(w http.ResponseWriter, r *http.Request) {
	var spec processSpec
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&spec); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "unreadable body: " + err.Error()})
		return
	}
	if err := appSupervisor.register(spec); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	// Saved only after it started. A declaration that cannot run is not worth
	// replaying on every boot from here on.
	if err := saveProcess(spec); err != nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok": true,
			"warning": "the process started but could not be recorded, so a cold " +
				"boot will not bring it back: " + err.Error(),
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleDeleteProcess stops a process and forgets it.
func handleDeleteProcess(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := appSupervisor.remove(name); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": err.Error()})
		return
	}
	if err := forgetProcess(name); err != nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":      true,
			"warning": "stopped, but the declaration is still on disk: " + err.Error(),
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleProcessAction starts, stops or restarts one process.
func handleProcessAction(w http.ResponseWriter, r *http.Request) {
	name, action := r.PathValue("name"), r.PathValue("action")
	var err error
	switch action {
	case "start":
		appSupervisor.mu.Lock()
		p := appSupervisor.procs[name]
		var spec processSpec
		if p != nil {
			spec = p.spec
		}
		appSupervisor.mu.Unlock()
		if p == nil {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "no process named " + name})
			return
		}
		if ok, msg := appSupervisor.startOne(spec); !ok {
			writeJSON(w, http.StatusConflict, map[string]any{"error": msg})
			return
		}
	case "stop":
		err = appSupervisor.stop(name)
	case "restart":
		err = appSupervisor.restart(name)
	default:
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": "unknown action " + action + "; want start, stop or restart"})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleProcessLogs returns one process's captured output.
func handleProcessLogs(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	tail := 0
	if v := r.URL.Query().Get("tail"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			tail = n
		}
	}
	out, err := appSupervisor.logsOf(name, tail)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": err.Error()})
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(out)
}
