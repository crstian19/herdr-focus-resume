// Package app runs focus resume for the Herdr desktop app, where terminals
// belong to herdr-core (hc) instead of a Herdr server with a session.json.
package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"time"

	"github.com/crstian19/herdr-focus-resume/internal/wake"
)

// Commands is the slice of hc and herdr-app the app mode needs.
type Commands interface {
	// Terminals lists herdr-core's live local terminals.
	Terminals() ([]Terminal, error)
	// Get returns one terminal with its labels and foreground process.
	Get(address string) (Terminal, error)
	// Label sets labels on a terminal; an empty value removes the key.
	Label(address string, labels map[string]string) error
	// Send pastes text into a terminal and presses Enter.
	Send(address, text string) error
	// Instance identifies the herdr-core holder that keeps terminals alive.
	// It changes when every terminal was lost (reboot, logout, core restart).
	Instance() (string, error)
	// FocusedTerminal is the terminal behind the app's focused pane, or "".
	FocusedTerminal() (string, error)

	// The rest backs the Herdr API the daemon serves to the herdr CLI.

	// Read returns a terminal's screen plus lines of history above it.
	Read(address string, lines int) (string, error)
	// Keys presses named keys (enter, esc, ctrl+c) in a terminal.
	Keys(address string, keys []string) error
	// Kill ends a terminal's program.
	Kill(address string) error
	// Layout is what the app window shows; it fails when the window is closed.
	Layout() (Layout, error)
	// AppDo runs a herdr-app command (pane.split, pane.close, terminal.open).
	AppDo(command string, args map[string]any) error
	// HerdrAPI is the version and socket protocol of the installed herdr CLI.
	HerdrAPI() (version string, protocol int, err error)
}

// Layout is the part of `herdr-app ls` and `herdr-app status` the Herdr API
// needs: which pane, tab and space show each terminal, and the focused one.
type Layout struct {
	Panes   []AppPane
	Focused string // terminal address of the focused pane, or ""
}

// AppPane is one terminal shown in the app window.
type AppPane struct {
	Terminal string
	Pane     string
	Tab      string
	Space    string
}

// Find returns the pane that shows terminal.
func (l Layout) Find(terminal string) (AppPane, bool) {
	for _, p := range l.Panes {
		if p.Terminal == terminal {
			return p, true
		}
	}
	return AppPane{}, false
}

// Terminal is the subset of `hc ls --json` and `hc get --json` the app mode
// reads.
type Terminal struct {
	Address    string            `json:"address"`
	Name       string            `json:"name"`
	Agent      string            `json:"agent"` // hc ls: the program, "claude-code" or "zsh"
	Cwd        string            `json:"cwd"`
	State      string            `json:"state"`
	Pid        int               `json:"pid"`
	Turn       uint64            `json:"turn"`
	Title      string            `json:"title"`
	Status     *Status           `json:"status"`
	Labels     map[string]string `json:"labels"`
	Foreground *struct {
		Name string `json:"name"`
		Pid  int    `json:"pid"`
	} `json:"foreground"`
}

// Status is what an agent reported to herdr-core. `hc get` gives an object
// ({"app":"omp","state":"working"}), `hc ls` only the state ("working").
type Status struct {
	App   string `json:"app"`
	State string `json:"state"`
}

func (s *Status) UnmarshalJSON(b []byte) error {
	var state string
	if err := json.Unmarshal(b, &state); err == nil {
		*s = Status{State: state}
		return nil
	}
	type plain Status
	return json.Unmarshal(b, (*plain)(s))
}

// CLI runs the hc and herdr-app binaries.
type CLI struct {
	HC      string // default "hc"
	App     string // default "herdr-app"
	Herdr   string // default "herdr"
	Timeout time.Duration
}

func (c CLI) run(bin string, args ...string) ([]byte, error) {
	timeout := c.Timeout
	if timeout == 0 {
		timeout = 10 * time.Second
	}
	return c.runFor(timeout, bin, args...)
}

func (c CLI) runFor(timeout time.Duration, bin string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("%s %v: %w: %s", bin, args, err, bytes.TrimSpace(stderr.Bytes()))
	}
	return stdout.Bytes(), nil
}

func (c CLI) hc(args ...string) ([]byte, error) {
	bin := c.HC
	if bin == "" {
		bin = "hc"
	}
	return c.run(bin, args...)
}

func (c CLI) app(args ...string) ([]byte, error) {
	bin := c.App
	if bin == "" {
		bin = "herdr-app"
	}
	return c.run(bin, args...)
}

func (c CLI) Terminals() ([]Terminal, error) {
	out, err := c.hc("ls", "--json")
	if err != nil {
		return nil, err
	}
	var resp struct {
		Terminals []Terminal `json:"terminals"`
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		return nil, fmt.Errorf("decode hc ls: %w", err)
	}
	return resp.Terminals, nil
}

func (c CLI) Get(address string) (Terminal, error) {
	var t Terminal
	out, err := c.hc("get", "--json", "--", address)
	if err != nil {
		return t, err
	}
	if err := json.Unmarshal(out, &t); err != nil {
		return t, fmt.Errorf("decode hc get: %w", err)
	}
	return t, nil
}

func (c CLI) Label(address string, labels map[string]string) error {
	args := []string{"label", address}
	for k, v := range labels {
		args = append(args, k+"="+v)
	}
	_, err := c.hc(args...)
	return err
}

// Send can wait up to a minute for a person to finish typing a draft in the
// terminal, so it gets more time than the other calls.
func (c CLI) Send(address, text string) error {
	_, err := c.runFor(90*time.Second, c.hcBin(), "send", "--", address, text)
	return err
}

func (c CLI) hcBin() string {
	if c.HC == "" {
		return "hc"
	}
	return c.HC
}

func (c CLI) Read(address string, lines int) (string, error) {
	out, err := c.hc("read", "--json", "--lines", strconv.Itoa(lines), "--", address)
	if err != nil {
		return "", err
	}
	var resp struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		return "", fmt.Errorf("decode hc read: %w", err)
	}
	return resp.Text, nil
}

func (c CLI) Keys(address string, keys []string) error {
	_, err := c.hc(append([]string{"keys", "--", address}, keys...)...)
	return err
}

func (c CLI) Kill(address string) error {
	_, err := c.hc("kill", "--", address)
	return err
}

func (c CLI) AppDo(command string, args map[string]any) error {
	b, err := json.Marshal(args)
	if err != nil {
		return err
	}
	_, err = c.app("do", command, string(b))
	return err
}

var protocolLine = regexp.MustCompile(`(?m)^protocol:\s*(\d+)`)

func (c CLI) HerdrAPI() (string, int, error) {
	bin := c.Herdr
	if bin == "" {
		bin = "herdr"
	}
	out, err := c.run(bin, "api", "schema")
	if err != nil {
		return "", 0, err
	}
	m := protocolLine.FindSubmatch(out)
	if m == nil {
		return "", 0, fmt.Errorf("herdr api schema: no protocol line")
	}
	protocol, err := strconv.Atoi(string(m[1]))
	if err != nil {
		return "", 0, err
	}
	out, err = c.run(bin, "--version")
	if err != nil {
		return "", 0, err
	}
	version := string(bytes.TrimSpace(bytes.TrimPrefix(bytes.TrimSpace(out), []byte("herdr"))))
	return version, protocol, nil
}

func (c CLI) Instance() (string, error) {
	out, err := c.hc("server", "status")
	if err != nil {
		return "", err
	}
	var resp struct {
		Holder struct {
			Instance string `json:"instance"`
		} `json:"holder"`
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		return "", fmt.Errorf("decode hc server status: %w", err)
	}
	if resp.Holder.Instance == "" {
		return "", fmt.Errorf("hc server status: no holder instance")
	}
	return resp.Holder.Instance, nil
}

func (c CLI) FocusedTerminal() (string, error) {
	out, err := c.app("status")
	if err != nil {
		return "", err
	}
	var status struct {
		FocusedPane string `json:"focused_pane"`
	}
	if err := json.Unmarshal(out, &status); err != nil {
		return "", fmt.Errorf("decode herdr-app status: %w", err)
	}
	if status.FocusedPane == "" {
		return "", nil // skip `herdr-app ls`: Watch calls this every 0.7 s
	}
	l, err := c.Layout()
	if err != nil {
		return "", err
	}
	return l.Focused, nil
}

func (c CLI) Layout() (Layout, error) {
	var l Layout
	out, err := c.app("status")
	if err != nil {
		return l, err
	}
	var status struct {
		FocusedPane string `json:"focused_pane"`
	}
	if err := json.Unmarshal(out, &status); err != nil {
		return l, fmt.Errorf("decode herdr-app status: %w", err)
	}
	out, err = c.app("ls")
	if err != nil {
		return l, err
	}
	var ls struct {
		Spaces []struct {
			ID   string `json:"id"`
			Tabs []struct {
				ID        string `json:"id"`
				Terminals []struct {
					Terminal string `json:"terminal"`
					Pane     string `json:"pane"`
				} `json:"terminals"`
			} `json:"tabs"`
		} `json:"spaces"`
	}
	if err := json.Unmarshal(out, &ls); err != nil {
		return l, fmt.Errorf("decode herdr-app ls: %w", err)
	}
	for _, s := range ls.Spaces {
		for _, t := range s.Tabs {
			for _, term := range t.Terminals {
				l.Panes = append(l.Panes, AppPane{Terminal: term.Terminal, Pane: term.Pane, Tab: t.ID, Space: s.ID})
				if status.FocusedPane != "" && term.Pane == status.FocusedPane {
					l.Focused = term.Terminal
				}
			}
		}
	}
	return l, nil
}

// adapter lets the original wake.Runner drive herdr-core terminals: a pane ID
// is a terminal address (t7) here.
type adapter struct {
	cmd     Commands
	focused string
}

var _ wake.Herdr = adapter{}

func (a adapter) ListPanes() ([]wake.Pane, error) {
	terms, err := a.cmd.Terminals()
	if err != nil {
		return nil, err
	}
	panes := make([]wake.Pane, 0, len(terms))
	for _, t := range terms {
		panes = append(panes, wake.Pane{ID: t.Address, Cwd: t.Cwd, Focused: t.Address == a.focused})
	}
	return panes, nil
}

// ShellIdle: the terminal's root process is its shell, so the shell is idle
// when it is also the foreground process.
func (a adapter) ShellIdle(address string) (bool, error) {
	t, err := a.cmd.Get(address)
	if err != nil {
		return false, err
	}
	return t.Pid != 0 && t.Foreground != nil && t.Foreground.Pid == t.Pid, nil
}

func (a adapter) Run(address, commandLine string) error {
	return a.cmd.Send(address, commandLine)
}
