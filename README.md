<p align="center">
  <img src="assets/logo.png" alt="A Go gopher in a nightcap waking up inside a focused terminal window, next to two dimmed windows with sleeping gophers" width="240">
</p>

<p align="center">
  <a href="https://github.com/crstian19/herdr-focus-resume/releases">
    <img src="https://img.shields.io/github/v/release/crstian19/herdr-focus-resume?sort=semver&style=for-the-badge" alt="Release">
  </a>
  <a href="https://github.com/crstian19/herdr-focus-resume/actions/workflows/ci.yml">
    <img src="https://img.shields.io/github/actions/workflow/status/crstian19/herdr-focus-resume/ci.yml?branch=main&label=CI&style=for-the-badge" alt="CI">
  </a>
  <a href="go.mod">
    <img src="https://img.shields.io/github/go-mod/go-version/crstian19/herdr-focus-resume?logo=go&logoColor=white&label=go&style=for-the-badge" alt="Go version">
  </a>
  <a href="https://herdr.dev">
    <img src="https://img.shields.io/badge/herdr-%E2%89%A5%200.9.2-blue?style=for-the-badge" alt="Herdr 0.9.2 or later">
  </a>
  <a href="LICENSE">
    <img src="https://img.shields.io/badge/license-MIT-blue?style=for-the-badge" alt="License: MIT">
  </a>
</p>

# herdr-focus-resume

A [Herdr](https://herdr.dev) plugin that resumes agent panes when you focus them, instead of all at
once when the server starts. It works with both the Herdr TUI and the Herdr desktop app.

After a server restart, Herdr resumes every agent pane it knows about (`claude --resume`,
`omp --resume`, `codex resume`, ...) as soon as a client attaches. With a dozen panes across
workspaces, that is a dozen agents booting together, each taking 400-800 MB of RAM, most of which
you will not open today. With this plugin only the focused pane resumes. The others stay as idle
shells (~7 MB each) until you select them.

The Herdr app has the opposite problem. When herdr-core loses its terminals (a reboot, a logout, a
core restart), every terminal comes back as a plain shell in its last folder, and no agent is
resumed at all. With this plugin, each one resumes when you focus it.

## Install: Herdr TUI

Requires Herdr 0.9.2 or later and Go to build.

```bash
herdr plugin install crstian19/herdr-focus-resume
```

To pin a release, add `--ref` with its tag, for example `--ref v0.1.0` (see
[releases](https://github.com/crstian19/herdr-focus-resume/releases)).

Turn off Herdr's own agent restore in `~/.config/herdr/config.toml`:

```toml
[session]
resume_agents_on_restore = false
```

Restart the Herdr server (`herdr server stop`, then `herdr`). Note that stopping the server ends
every process running in its panes.

## Install: Herdr app

Requires the Herdr app with herdr-core, Go to build, and Herdr's agent integrations.

1. Install the binary somewhere on your `PATH`:

   ```bash
   go install github.com/crstian19/herdr-focus-resume@latest
   ```

2. Install Herdr's integration for each agent you use. They are the same ones the TUI uses, and the
   plugin reads their session reports. This needs the `herdr` CLI:

   ```bash
   herdr integration install claude    # also: omp, codex, pi, opencode, kimi, ...
   herdr integration status
   ```

3. Add one line to your shell rc. It only acts in a terminal of the Herdr app (`HC_TERMINAL_ID` is
   set) and never inside the Herdr TUI:

   ```bash
   # ~/.zshrc or ~/.bashrc
   eval "$(herdr-focus-resume app env)"
   ```

   ```fish
   # ~/.config/fish/config.fish
   herdr-focus-resume app env fish | source
   ```

Open a new terminal in the app, or restart your agents, so they pick up the variables. There is no
mod to add and no layout to change.

## How it works

### Herdr TUI

With `resume_agents_on_restore = false`, Herdr restores panes as plain shells but keeps each pane's
agent session reference in `session.json`.

- **Startup hook:** reads `session.json`, records every pane that had an agent, and resumes the
  pane that already has focus.
- **`pane.focused` hook:** when you select a recorded pane, checks that its shell is idle and its
  working directory has not changed, then types the resume command into it. Each pane is resumed
  once per server start.

The resume command is the one the agent reported to Herdr (`agent_resume`) or, for Herdr's built-in
integrations, the command from Herdr's documentation: `claude`, `codex`, `omp`, `pi`, `opencode`,
`kilo`, `kimi`, `grok`, `copilot`, `devin`, `droid`, `qwen`, `hermes` and `mastracode`. See
`internal/wake/resume.go`.

### Herdr app

The app has no plugin hooks and no `session.json`, so the plugin brings its own pieces:

- **Session reports:** the rc line points Herdr's integrations at a small daemon, through
  `HERDR_ENV`, `HERDR_PANE_ID` (the terminal's `HC_TERMINAL_ID`) and `HERDR_SOCKET_PATH`. The daemon
  speaks the same protocol as Herdr (`pane.report_agent_session`). It stores each session as labels
  on the terminal (`focus-resume.agent`, `.session`, `.cwd` and `.instance`). Labels survive a
  herdr-core restart.
- **Startup:** the rc line also runs `herdr-focus-resume app ensure`, which starts the daemon if it
  isn't running. The first shell after the terminals were lost builds that boot's list of dormant
  terminals. These are the ones whose session was recorded under an earlier herdr-core holder
  instance. The focused terminal resumes right away.
- **Focus:** the daemon reads the app's focused terminal with `herdr-app status`, every 0.7 s while
  the window is open. When you select a dormant terminal, it checks that the shell is idle and the
  folder hasn't changed, then types the resume command with `hc send`. Each terminal resumes once
  per boot.

`herdr-focus-resume app focus <t>` resumes a terminal by hand.

### The herdr CLI inside the app

`HERDR_SOCKET_PATH` points the `herdr` CLI at the daemon too, so the daemon also answers the CLI's
pane and agent commands. It translates them to herdr-core (`hc`) and the app window (`herdr-app`).
A pane ID is a terminal address (`t7`), the same as `HERDR_PANE_ID`.

| `herdr` command | Runs |
| --- | --- |
| `pane list`, `pane current`, `pane get`, `agent list`, `agent get` | `hc ls` / `hc get`, plus `herdr-app ls` for spaces, tabs and focus |
| `pane read`, `agent read` | `hc read --lines N` |
| `pane run <p> <text>`, `agent prompt` | `hc send` (pastes, then presses Enter) |
| `pane send-keys`, `agent send-keys` | `hc keys` (`C-c` becomes `ctrl+c`) |
| `agent wait`, `agent prompt --wait`, `pane wait-output` | polls `hc get` / `hc read` |
| `pane split` | `herdr-app do pane.split`. With `--cwd`, only next to the focused pane (`terminal.open`) |
| `pane close` | `herdr-app do pane.close`, or `hc kill` for a terminal without a pane |

Other commands fail with `unsupported` and name the `hc` or `herdr-app` command to use instead.
For example, `pane send-text` cannot work, because herdr-core has no way to paste without
pressing Enter. Workspaces and tabs belong to the app window (`herdr-app do space.new`, `tab.new`).

## Safety

- If `resume_agents_on_restore` is not `false`, the plugin does nothing, so an agent is never
  started twice. It logs that it is inactive at startup.
- If something other than the shell is running in a pane, the pane is left alone and dropped from
  the list.
- Focus events that arrive at the same time are serialised with a file lock.
- Resume commands are validated with Herdr's own rules (plain command name, no control characters)
  and shell-quoted before they are typed. The command name is left unquoted when it is a plain word,
  so your shell aliases and functions for it apply (for example `alias omp='omp --allow-home'`).
- In the app, only local terminals are touched. Terminals on other machines (`vps/t7`) are skipped,
  and sessions recorded during the current boot are never resumed.

## Logs

Logs and per-session state are in Herdr's plugin state directory, usually
`~/.local/state/herdr/plugins/crstian19.focus-resume/`:

```text
startup: 10 dormant pane(s)
woke wE:p2B (claude) in /home/me/project
woke wE:p1V (omp) in /home/me/other
```

In the app, the daemon logs to `~/.local/state/herdr-focus-resume/app.log`.

## Limitations

- Pane ids are derived from `session.json` (`public_pane_numbers`, encoded in Crockford base32).
  This is an internal Herdr format. If it changes, the working-directory check makes the plugin
  skip the pane rather than type into the wrong one, and the log shows the skipped panes.
- Tested on Linux with Herdr 0.9.5, Claude Code and omp. The other agents use the documented
  commands but have not been tested.
- The resume command shows up in the shell history of the pane.
- In the app, the rc line sets `HERDR_ENV=1`, so tools that check it think they run inside the Herdr
  TUI. Running `herdr` commands from an app terminal reaches the plugin's daemon, which only answers
  integration reports.
- In the app, if you quit an agent and keep its shell, that shell still holds the last session. After
  the next reboot it resumes when you focus it.
- The app support was tested on Linux with Herdr app 0.1.18 and herdr-core 0.1.22, using Claude Code's
  integration end to end.

## Development

```bash
go build -o bin/herdr-focus-resume .
herdr plugin link "$PWD"

go vet ./... && go test -race ./...
go test -run '^$' -fuzz FuzzShellQuoteRoundTrip -fuzztime 30s ./internal/wake
```

## License

MIT. The logo is based on the Go gopher by Renée French, licensed under
[CC BY 4.0](https://creativecommons.org/licenses/by/4.0/).
