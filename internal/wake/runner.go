package wake

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// Pane is the subset of Herdr's live pane record the runner needs.
type Pane struct {
	ID      string `json:"pane_id"`
	Cwd     string `json:"cwd"`
	Focused bool   `json:"focused"`
	Agent   string `json:"agent"`
}

// Herdr is the slice of the Herdr CLI the runner talks to.
type Herdr interface {
	ListPanes() ([]Pane, error)
	// ShellIdle reports whether the pane's foreground process is its shell.
	ShellIdle(paneID string) (bool, error)
	// Run types the command line into the pane and presses Enter.
	Run(paneID, commandLine string) error
}

// Runner keeps the dormant-pane ledger for one Herdr session.
type Runner struct {
	Herdr      Herdr
	LedgerPath string
	Log        func(format string, args ...any)
	// IdleWait bounds how long a freshly restored shell may take to settle.
	IdleWait time.Duration
	Sleep    func(time.Duration)
}

func (r *Runner) logf(format string, args ...any) {
	if r.Log != nil {
		r.Log(format, args...)
	}
}

// Startup replaces the ledger with the dormant panes from session.json and
// wakes the pane that already has focus, since Herdr will not emit
// pane.focused for it.
func (r *Runner) Startup(dormant []Dormant) error {
	live, err := r.Herdr.ListPanes()
	if err != nil {
		return fmt.Errorf("list panes: %w", err)
	}
	byID := make(map[string]Pane, len(live))
	for _, p := range live {
		byID[p.ID] = p
	}
	ledger := map[string]Dormant{}
	for _, d := range dormant {
		p, ok := byID[d.PaneID]
		if !ok {
			r.logf("skip %s: pane not found after restore", d.PaneID)
			continue
		}
		if p.Cwd != d.Cwd {
			r.logf("skip %s: cwd %q does not match saved %q", d.PaneID, p.Cwd, d.Cwd)
			continue
		}
		ledger[d.PaneID] = d
	}
	r.logf("startup: %d dormant pane(s)", len(ledger))

	var focused string
	for _, p := range live {
		if p.Focused {
			focused = p.ID
		}
	}
	return r.withLedger(func(l map[string]Dormant) (map[string]Dormant, error) {
		if focused != "" {
			if d, ok := ledger[focused]; ok {
				if r.wake(d) {
					delete(ledger, focused)
				}
			}
		}
		return ledger, nil
	})
}

// Focus wakes paneID if it is still dormant.
func (r *Runner) Focus(paneID string) error {
	return r.withLedger(func(l map[string]Dormant) (map[string]Dormant, error) {
		d, ok := l[paneID]
		if !ok {
			return l, nil
		}
		if r.wake(d) {
			delete(l, paneID)
		}
		return l, nil
	})
}

// wake resumes d and reports whether the ledger entry is finished with,
// either because the agent started or because the pane is no longer ours.
func (r *Runner) wake(d Dormant) bool {
	deadline := r.IdleWait
	for waited := time.Duration(0); ; waited += 200 * time.Millisecond {
		idle, err := r.Herdr.ShellIdle(d.PaneID)
		if err != nil {
			r.logf("wake %s: %v", d.PaneID, err)
			return false
		}
		if idle {
			break
		}
		if waited >= deadline {
			// Something else is running there: the user took the pane over.
			r.logf("wake %s: pane busy, dropping it", d.PaneID)
			return true
		}
		r.sleep(200 * time.Millisecond)
	}
	if err := r.Herdr.Run(d.PaneID, CommandLine(d.Argv)); err != nil {
		r.logf("wake %s: %v", d.PaneID, err)
		return false
	}
	r.logf("woke %s (%s) in %s", d.PaneID, d.Agent, d.Cwd)
	return true
}

func (r *Runner) sleep(d time.Duration) {
	if r.Sleep != nil {
		r.Sleep(d)
		return
	}
	time.Sleep(d)
}

// withLedger runs fn under an exclusive lock so overlapping focus events
// cannot resume the same pane twice.
func (r *Runner) withLedger(fn func(map[string]Dormant) (map[string]Dormant, error)) error {
	if err := os.MkdirAll(filepath.Dir(r.LedgerPath), 0o700); err != nil {
		return err
	}
	unlock, err := lockFile(r.LedgerPath + ".lock")
	if err != nil {
		return err
	}
	defer unlock()

	l := map[string]Dormant{}
	b, err := os.ReadFile(r.LedgerPath)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return err
	default:
		if err := json.Unmarshal(b, &l); err != nil {
			r.logf("ledger unreadable, starting empty: %v", err)
			l = map[string]Dormant{}
		}
	}
	next, err := fn(l)
	if err != nil {
		return err
	}
	out, err := json.MarshalIndent(next, "", "  ")
	if err != nil {
		return err
	}
	tmp := r.LedgerPath + ".tmp"
	if err := os.WriteFile(tmp, out, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, r.LedgerPath)
}
