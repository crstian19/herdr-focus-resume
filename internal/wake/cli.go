package wake

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"time"
)

// CLI drives Herdr through its own binary, the portable path plugins are
// meant to use (HERDR_BIN_PATH).
type CLI struct {
	Bin     string
	Timeout time.Duration
}

func (c CLI) call(args ...string) ([]byte, error) {
	timeout := c.Timeout
	if timeout == 0 {
		timeout = 5 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, c.Bin, args...)
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("herdr %v: %w: %s", args, err, bytes.TrimSpace(stderr.Bytes()))
	}
	return out, nil
}

func (c CLI) ListPanes() ([]Pane, error) {
	out, err := c.call("pane", "list")
	if err != nil {
		return nil, err
	}
	var resp struct {
		Result struct {
			Panes []Pane `json:"panes"`
		} `json:"result"`
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		return nil, fmt.Errorf("decode pane list: %w", err)
	}
	return resp.Result.Panes, nil
}

func (c CLI) ShellIdle(paneID string) (bool, error) {
	out, err := c.call("pane", "process-info", "--pane", paneID)
	if err != nil {
		return false, err
	}
	var resp struct {
		Result struct {
			ProcessInfo struct {
				ForegroundPGID int `json:"foreground_process_group_id"`
				ShellPID       int `json:"shell_pid"`
			} `json:"process_info"`
		} `json:"result"`
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		return false, fmt.Errorf("decode process info: %w", err)
	}
	pi := resp.Result.ProcessInfo
	return pi.ShellPID != 0 && pi.ForegroundPGID == pi.ShellPID, nil
}

func (c CLI) Run(paneID, commandLine string) error {
	if _, err := c.call("pane", "send-text", paneID, commandLine); err != nil {
		return err
	}
	_, err := c.call("pane", "send-keys", paneID, "Enter")
	return err
}
