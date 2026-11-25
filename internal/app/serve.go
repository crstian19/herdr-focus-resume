package app

import (
	"bufio"
	"encoding/json"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// SocketPath is where the daemon listens. Shells inside the app point Herdr's
// integrations at it through HERDR_SOCKET_PATH. It lives in XDG_RUNTIME_DIR
// (short, per login) when there is one, and is named after herdr-core's
// session so each core gets its own daemon.
func SocketPath(stateDir string) string {
	name := "herdr-focus-resume.sock"
	if s := os.Getenv("HC_SESSION"); s != "" && s != "default" {
		name = "herdr-focus-resume-" + s + ".sock"
	}
	if dir := os.Getenv("XDG_RUNTIME_DIR"); dir != "" {
		return filepath.Join(dir, name)
	}
	return filepath.Join(stateDir, name)
}

type request struct {
	ID     string          `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

// Serve answers Herdr integration requests on the socket: it records
// pane.report_agent_session and acknowledges the other report methods.
func (a *App) Serve(l net.Listener) error {
	for {
		conn, err := l.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		go a.handle(conn)
	}
}

func (a *App) handle(conn net.Conn) {
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(10 * time.Second))
	sc := bufio.NewScanner(conn)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	for sc.Scan() {
		var req request
		if err := json.Unmarshal(sc.Bytes(), &req); err != nil {
			reply(conn, "", nil, "invalid_request", "invalid JSON")
			continue
		}
		switch req.Method {
		case "pane.report_agent_session":
			var r Report
			if err := json.Unmarshal(req.Params, &r); err != nil {
				reply(conn, req.ID, nil, "invalid_params", err.Error())
				continue
			}
			// Answer first: integrations wait at most ~0.5 s for the reply.
			reply(conn, req.ID, map[string]string{"type": "ok"}, "", "")
			if err := a.Record(r); err != nil {
				a.logf("record %s: %v", r.PaneID, err)
			}
		case "pane.report_agent", "pane.report_metadata":
			reply(conn, req.ID, map[string]string{"type": "ok"}, "", "")
		default:
			reply(conn, req.ID, nil, "unsupported", "herdr-focus-resume only takes integration reports")
		}
	}
}

func reply(conn net.Conn, id string, result any, code, message string) {
	resp := map[string]any{"id": id}
	if code != "" {
		resp["error"] = map[string]string{"code": code, "message": message}
	} else {
		resp["result"] = result
	}
	b, _ := json.Marshal(resp)
	conn.Write(append(b, '\n'))
}

// Listen opens the socket, replacing a stale one. It reports running=true when
// another daemon already answers there.
func Listen(path string) (l net.Listener, running bool, err error) {
	if Alive(path) {
		return nil, true, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, false, err
	}
	os.Remove(path)
	l, err = net.Listen("unix", path)
	if err != nil {
		return nil, false, err
	}
	os.Chmod(path, 0o600)
	return l, false, nil
}

// Alive reports whether a daemon answers on the socket.
func Alive(path string) bool {
	c, err := net.DialTimeout("unix", path, 300*time.Millisecond)
	if err != nil {
		return false
	}
	c.Close()
	return true
}

// Spawn starts `<self> app serve` in its own session so it outlives the shell
// that ran `app ensure`. Terminal-specific variables are dropped.
func Spawn(self, logPath string) error {
	logf, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer logf.Close()
	cmd := exec.Command(self, "app", "serve")
	cmd.Stdout, cmd.Stderr = logf, logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		switch k {
		case "HC_TERMINAL_ID", "HERDR_ENV", "HERDR_PANE_ID", "HERDR_SOCKET_PATH":
			continue
		}
		cmd.Env = append(cmd.Env, kv)
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}
