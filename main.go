// Command herdr-focus-resume is a Herdr plugin hook: after a server restart it
// resumes each agent pane only when that pane is focused.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/crstian19/herdr-focus-resume/internal/wake"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "app" {
		if err := runApp(os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "herdr-focus-resume: %v\n", err)
			os.Exit(1)
		}
		return
	}
	logger := openLog()
	if err := run(logger); err != nil {
		logger.Printf("error: %v", err)
		os.Exit(1)
	}
}

func run(logger *log.Logger) error {
	socket := os.Getenv("HERDR_SOCKET_PATH")
	stateDir := os.Getenv("HERDR_PLUGIN_STATE_DIR")
	if socket == "" || stateDir == "" {
		return fmt.Errorf("not running as a Herdr plugin (HERDR_SOCKET_PATH/HERDR_PLUGIN_STATE_DIR unset)")
	}
	sessionDir := filepath.Dir(socket)

	if !resumeDisabled() {
		// Herdr resumes every agent itself; acting too would start them twice.
		if os.Getenv("HERDR_PLUGIN_EVENT") == "startup" {
			logger.Printf("inactive: set [session] resume_agents_on_restore = false in %s to enable focus resume", configPath())
		}
		return nil
	}

	bin := os.Getenv("HERDR_BIN_PATH")
	if bin == "" {
		bin = "herdr"
	}
	r := &wake.Runner{
		Herdr:      wake.CLI{Bin: bin},
		LedgerPath: filepath.Join(stateDir, "ledger-"+sessionKey(sessionDir)+".json"),
		Log:        logger.Printf,
		IdleWait:   5 * time.Second,
	}

	switch ev := os.Getenv("HERDR_PLUGIN_EVENT"); ev {
	case "startup":
		f, err := os.Open(filepath.Join(sessionDir, "session.json"))
		if err != nil {
			return err
		}
		defer f.Close()
		dormant, skipped, err := wake.ParseSession(f)
		if err != nil {
			return err
		}
		for _, s := range skipped {
			logger.Printf("skip %s: %s", s.PaneID, s.Reason)
		}
		return r.Startup(dormant)
	case "pane.focused":
		var payload struct {
			Data struct {
				PaneID string `json:"pane_id"`
			} `json:"data"`
		}
		if err := json.Unmarshal([]byte(os.Getenv("HERDR_PLUGIN_EVENT_JSON")), &payload); err != nil {
			return fmt.Errorf("decode event: %w", err)
		}
		if payload.Data.PaneID == "" {
			return fmt.Errorf("pane.focused event without pane_id")
		}
		return r.Focus(payload.Data.PaneID)
	default:
		return fmt.Errorf("unexpected event %q", ev)
	}
}

func configPath() string {
	if path := os.Getenv("HERDR_CONFIG_PATH"); path != "" {
		return path
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "herdr", "config.toml")
}

func resumeDisabled() bool {
	path := configPath()
	if path == "" {
		return false
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	return wake.ResumeOnRestoreDisabled(bytes.NewReader(b))
}

// sessionKey separates ledgers per Herdr session; plugins are global.
func sessionKey(sessionDir string) string {
	h := fnv.New64a()
	h.Write([]byte(sessionDir))
	return fmt.Sprintf("%016x", h.Sum64())
}

func openLog() *log.Logger {
	dir := os.Getenv("HERDR_PLUGIN_STATE_DIR")
	if dir != "" {
		if f, err := os.OpenFile(filepath.Join(dir, "focus-resume.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600); err == nil {
			return log.New(f, "", log.LstdFlags)
		}
	}
	return log.New(os.Stderr, "", log.LstdFlags)
}
