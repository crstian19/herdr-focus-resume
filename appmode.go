package main

import (
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/crstian19/herdr-focus-resume/internal/app"
)

const appUsage = `usage: herdr-focus-resume app <command>

Focus resume for the Herdr desktop app (herdr-app + herdr-core).

  env [sh|fish]   print the shell lines that point Herdr's integrations and
                  the herdr CLI at the daemon and start it; meant for eval in
                  your shell rc
  ensure          start the daemon if it isn't running and build this boot's
                  list of dormant terminals
  serve           run the daemon in the foreground
  focus <t>       resume terminal <t> now if it is dormant
`

func runApp(args []string) error {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, appUsage)
		return fmt.Errorf("missing command")
	}
	stateDir, err := app.DefaultStateDir()
	if err != nil {
		return err
	}
	logger := log.New(os.Stderr, "", log.LstdFlags)
	a := &app.App{Cmd: app.CLI{}, StateDir: stateDir, Log: logger.Printf, IdleWait: 5 * time.Second}
	sock := app.SocketPath(stateDir)

	switch args[0] {
	case "env":
		shell := "sh"
		if len(args) > 1 {
			shell = args[1]
		}
		return printEnv(shell, sock)
	case "ensure":
		if err := os.MkdirAll(stateDir, 0o700); err != nil {
			return err
		}
		if !app.Alive(sock) {
			// The new daemon prepares this boot's ledger itself.
			self, err := os.Executable()
			if err != nil {
				return err
			}
			return app.Spawn(self, filepath.Join(stateDir, "app.log"))
		}
		// The daemon outlived a herdr-core restart: the first shell of the new
		// holder instance builds its ledger (a no-op once built).
		_, err := a.Prepare()
		return err
	case "serve":
		l, running, err := app.Listen(sock)
		if err != nil {
			return err
		}
		if running {
			return nil
		}
		defer os.Remove(sock)
		if _, err := a.Prepare(); err != nil {
			logger.Printf("prepare: %v", err)
		}
		stop := make(chan struct{})
		go a.Watch(700*time.Millisecond, stop)
		go func() {
			sig := make(chan os.Signal, 1)
			signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
			<-sig
			close(stop)
			l.Close()
		}()
		logger.Printf("listening on %s", sock)
		return a.Serve(l)
	case "focus":
		if len(args) < 2 {
			return fmt.Errorf("usage: herdr-focus-resume app focus <terminal>")
		}
		return a.Focus(args[1])
	default:
		fmt.Fprint(os.Stderr, appUsage)
		return fmt.Errorf("unknown command %q", args[0])
	}
}

// printEnv writes shell code that only acts inside a herdr-core terminal of
// the app (HC_TERMINAL_ID) and never inside the Herdr TUI (HERDR_ENV already set).
// Integrations and the herdr CLI both follow HERDR_SOCKET_PATH, so the daemon
// answers both (see app.App.Serve).
func printEnv(shell, sock string) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	switch shell {
	case "sh", "bash", "zsh":
		fmt.Printf(`if [ -n "${HC_TERMINAL_ID-}" ] && [ -z "${HERDR_ENV-}" ]; then
  export HERDR_ENV=1 HERDR_PANE_ID="$HC_TERMINAL_ID" HERDR_SOCKET_PATH=%q
  (%q app ensure >/dev/null 2>&1 &)
fi
`, sock, self)
	case "fish":
		fmt.Printf(`if set -q HC_TERMINAL_ID; and not set -q HERDR_ENV
  set -gx HERDR_ENV 1
  set -gx HERDR_PANE_ID $HC_TERMINAL_ID
  set -gx HERDR_SOCKET_PATH %q
  %q app ensure >/dev/null 2>&1 &
  disown 2>/dev/null
end
`, sock, self)
	default:
		return fmt.Errorf("unknown shell %q (use sh or fish)", shell)
	}
	return nil
}
