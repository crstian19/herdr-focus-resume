// Package app runs focus resume for the Herdr desktop app, where terminals
// belong to herdr-core (hc) instead of a Herdr server with a session.json.
package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
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
}

// Terminal is the subset of `hc get --json` the app mode reads.
type Terminal struct {
	Address    string            `json:"address"`
	Cwd        string            `json:"cwd"`
	State      string            `json:"state"`
	Pid        int               `json:"pid"`
	Labels     map[string]string `json:"labels"`
	Foreground *struct {
		Name string `json:"name"`
		Pid  int    `json:"pid"`
	} `json:"foreground"`
}

// CLI runs the hc and herdr-app binaries.
type CLI struct {
	HC      string // default "hc"
	App     string // default "herdr-app"
	Timeout time.Duration
}

func (c CLI) run(bin string, args ...string) ([]byte, error) {
	timeout := c.Timeout
	if timeout == 0 {
		timeout = 10 * time.Second
	}
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
	out, err := c.hc("get", address, "--json")
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

func (c CLI) Send(address, text string) error {
	_, err := c.hc("send", address, text)
	return err
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
		return "", nil
	}
	out, err = c.app("ls")
	if err != nil {
		return "", err
	}
	var ls struct {
		Spaces []struct {
			Tabs []struct {
				Terminals []struct {
					Terminal string `json:"terminal"`
					Pane     string `json:"pane"`
				} `json:"terminals"`
			} `json:"tabs"`
		} `json:"spaces"`
	}
	if err := json.Unmarshal(out, &ls); err != nil {
		return "", fmt.Errorf("decode herdr-app ls: %w", err)
	}
	for _, s := range ls.Spaces {
		for _, t := range s.Tabs {
			for _, term := range t.Terminals {
				if term.Pane == status.FocusedPane {
					return term.Terminal, nil
				}
			}
		}
	}
	return "", nil
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
