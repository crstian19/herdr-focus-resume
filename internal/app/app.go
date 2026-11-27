package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/crstian19/herdr-focus-resume/internal/wake"
)

// Labels kept on each herdr-core terminal. They survive a core restart, which
// is what lets a restored shell know which agent session it used to hold.
const (
	LabelAgent    = "focus-resume.agent"
	LabelSession  = "focus-resume.session"
	LabelArgv     = "focus-resume.argv"
	LabelCwd      = "focus-resume.cwd"
	LabelInstance = "focus-resume.instance"
)

// Report is the part of a Herdr integration's pane.report_agent_session
// request the app mode stores.
type Report struct {
	PaneID      string            `json:"pane_id"`
	Agent       string            `json:"agent"`
	SessionID   string            `json:"agent_session_id"`
	SessionPath string            `json:"agent_session_path"`
	Resume      *wake.AgentResume `json:"agent_resume"`
}

// App holds what every app-mode command shares.
type App struct {
	Cmd      Commands
	StateDir string
	Log      func(format string, args ...any)
	IdleWait time.Duration
	// Poll is how often Herdr API waits check a terminal (default 300 ms).
	Poll time.Duration
}

func (a *App) logf(format string, args ...any) {
	if a.Log != nil {
		a.Log(format, args...)
	}
}

// Record stores an integration's session report on its terminal, tagged with
// the current holder instance so a later instance knows it is dormant.
func (a *App) Record(r Report) error {
	if r.PaneID == "" || strings.Contains(r.PaneID, "/") {
		return nil // not a local herdr-core terminal
	}
	value := r.SessionID
	if value == "" {
		value = r.SessionPath
	}
	session := &wake.AgentSession{Agent: r.Agent, Value: value}
	if _, err := wake.ResumeArgv(r.Resume, session); err != nil {
		a.logf("ignore report for %s (%s): %v", r.PaneID, r.Agent, err)
		return nil
	}
	t, err := a.Cmd.Get(r.PaneID)
	if err != nil {
		return err
	}
	instance, err := a.Cmd.Instance()
	if err != nil {
		return err
	}
	labels := map[string]string{
		LabelAgent:    r.Agent,
		LabelSession:  value,
		LabelCwd:      t.Cwd,
		LabelInstance: instance,
		LabelArgv:     "",
	}
	if r.Resume != nil && len(r.Resume.Argv) > 0 {
		b, err := json.Marshal(r.Resume.Argv)
		if err != nil {
			return err
		}
		labels[LabelArgv] = string(b)
	}
	if err := a.Cmd.Label(r.PaneID, labels); err != nil {
		return err
	}
	a.logf("recorded %s: %s session in %s", r.PaneID, r.Agent, t.Cwd)
	return nil
}

// dormant lists the terminals whose recorded session belongs to an earlier
// holder instance: their agent was lost and they are plain shells again.
func (a *App) dormant(instance string) ([]wake.Dormant, error) {
	terms, err := a.Cmd.Terminals()
	if err != nil {
		return nil, err
	}
	var out []wake.Dormant
	for _, lt := range terms {
		if lt.State != "" && lt.State != "live" {
			continue
		}
		t, err := a.Cmd.Get(lt.Address)
		if err != nil {
			a.logf("skip %s: %v", lt.Address, err)
			continue
		}
		l := t.Labels
		if l[LabelSession] == "" || l[LabelInstance] == instance {
			continue
		}
		var resume *wake.AgentResume
		if l[LabelArgv] != "" {
			var argv []string
			if err := json.Unmarshal([]byte(l[LabelArgv]), &argv); err == nil {
				resume = &wake.AgentResume{Agent: l[LabelAgent], Argv: argv}
			}
		}
		argv, err := wake.ResumeArgv(resume, &wake.AgentSession{Agent: l[LabelAgent], Value: l[LabelSession]})
		if err != nil {
			a.logf("skip %s: %v", t.Address, err)
			continue
		}
		out = append(out, wake.Dormant{PaneID: t.Address, Cwd: l[LabelCwd], Agent: l[LabelAgent], Argv: argv})
	}
	return out, nil
}

func (a *App) runner(instance, focused string) *wake.Runner {
	return &wake.Runner{
		Herdr:      adapter{cmd: a.Cmd, focused: focused},
		LedgerPath: filepath.Join(a.StateDir, "ledger-"+instance+".json"),
		Log:        a.logf,
		IdleWait:   a.IdleWait,
	}
}

// Prepare builds the dormant ledger once per holder instance and wakes the
// focused terminal, like the TUI plugin's startup hook.
func (a *App) Prepare() (string, error) {
	instance, err := a.Cmd.Instance()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(a.StateDir, 0o700); err != nil {
		return "", err
	}
	marker := filepath.Join(a.StateDir, "started-"+instance)
	f, err := os.OpenFile(marker, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if errors.Is(err, fs.ErrExist) {
		return instance, nil
	}
	if err != nil {
		return "", err
	}
	f.Close()
	a.prune(instance)
	dormant, err := a.dormant(instance)
	if err != nil {
		return "", err
	}
	focused, err := a.Cmd.FocusedTerminal()
	if err != nil {
		focused = "" // the app window may not be open yet
	}
	a.logf("instance %s", instance)
	return instance, a.runner(instance, focused).Startup(dormant)
}

// Focus wakes address if it is still dormant in this instance.
func (a *App) Focus(address string) error {
	if address == "" || strings.Contains(address, "/") {
		return nil
	}
	instance, err := a.Prepare()
	if err != nil {
		return err
	}
	return a.runner(instance, address).Focus(address)
}

// prune drops the ledgers and markers of earlier instances.
func (a *App) prune(keep string) {
	entries, err := os.ReadDir(a.StateDir)
	if err != nil {
		return
	}
	for _, e := range entries {
		name := e.Name()
		if strings.Contains(name, keep) {
			continue
		}
		if strings.HasPrefix(name, "started-") || strings.HasPrefix(name, "ledger-") {
			os.Remove(filepath.Join(a.StateDir, name))
		}
	}
}

// Watch polls the app's focused terminal and wakes it when it changes. It
// returns when stop is closed.
func (a *App) Watch(interval time.Duration, stop <-chan struct{}) {
	last := ""
	wait := interval
	for {
		select {
		case <-stop:
			return
		case <-time.After(wait):
		}
		focused, err := a.Cmd.FocusedTerminal()
		if err != nil {
			wait = 5 * time.Second // the app window is closed
			continue
		}
		wait = interval
		if focused == last {
			continue
		}
		last = focused
		if err := a.Focus(focused); err != nil {
			a.logf("focus %s: %v", focused, err)
		}
	}
}

// DefaultStateDir is $XDG_STATE_HOME/herdr-focus-resume.
func DefaultStateDir() (string, error) {
	if dir := os.Getenv("XDG_STATE_HOME"); dir != "" {
		return filepath.Join(dir, "herdr-focus-resume"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("no state dir: %w", err)
	}
	return filepath.Join(home, ".local", "state", "herdr-focus-resume"), nil
}
