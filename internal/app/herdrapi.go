package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"regexp"
	"slices"
	"strings"
	"time"
)

// The app's shells point HERDR_SOCKET_PATH at the daemon so Herdr's agent
// integrations report to it. The herdr CLI reads the same variable, so the
// daemon also answers the CLI's pane and agent requests, translated to
// herdr-core (hc) and the app window (herdr-app). A Herdr pane ID here is a
// terminal address (t7), as in HERDR_PANE_ID.

// apiError is an error reply with a Herdr error code.
type apiError struct {
	Code    string
	Message string
}

func (e *apiError) Error() string { return e.Message }

func errf(code, format string, args ...any) *apiError {
	return &apiError{Code: code, Message: fmt.Sprintf(format, args...)}
}

// hints name the hc or herdr-app command for Herdr API methods the daemon
// can't translate.
var hints = map[string]string{
	"pane.send_text":    "herdr-core can't paste without pressing Enter; use `herdr pane run <pane> <text>` or `hc send <t> <text>` (pastes and presses Enter)",
	"agent.start":       "use `hc run --bg <agent>` or `herdr-app do terminal.open --argv '[\"<agent>\"]'`",
	"agent.focus":       "use `herdr-app do agent.focus --terminal <t>`",
	"pane.focus":        "use `herdr-app do agent.focus --terminal <t>`",
	"pane.rename":       "use `hc label <t> name=<name>` or herdr-app's rename commands (`herdr-app commands`)",
	"agent.rename":      "use `hc label <t> name=<name>` or herdr-app's rename commands (`herdr-app commands`)",
	"pane.process_info": "use `hc get <t>` (foreground process and pid)",
}

func unsupported(method string) *apiError {
	hint, ok := hints[method]
	if !ok {
		switch {
		case strings.HasPrefix(method, "workspace."), strings.HasPrefix(method, "tab."), strings.HasPrefix(method, "worktree."):
			hint = "spaces and tabs belong to the app window; use `herdr-app do space.new|tab.new|...` (`herdr-app commands`)"
		default:
			hint = "use hc (`hc --help`) for terminals and herdr-app (`herdr-app commands`) for the window"
		}
	}
	return errf("unsupported", "%s is not available in the Herdr app (herdr-focus-resume translates pane list/current/get/read/split/close/run/send-keys/wait-output and agent list/get/read/prompt/wait/send-keys); %s", method, hint)
}

// paneInfo is Herdr's PaneInfo and AgentInfo in one: the CLI prints it as is.
type paneInfo struct {
	PaneID      string  `json:"pane_id"`
	TerminalID  string  `json:"terminal_id"`
	WorkspaceID string  `json:"workspace_id"`
	TabID       string  `json:"tab_id"`
	Focused     bool    `json:"focused"`
	AgentStatus string  `json:"agent_status"`
	Revision    uint64  `json:"revision"`
	Agent       *string `json:"agent"`
	Name        *string `json:"name,omitempty"`
	Cwd         string  `json:"cwd,omitempty"`
	Title       string  `json:"title,omitempty"`
}

type readResult struct {
	PaneID      string `json:"pane_id"`
	WorkspaceID string `json:"workspace_id"`
	TabID       string `json:"tab_id"`
	Source      string `json:"source"`
	Format      string `json:"format"`
	Text        string `json:"text"`
	Revision    uint64 `json:"revision"`
	Truncated   bool   `json:"truncated"`
}

// herdrAgent maps herdr-core's app names to Herdr's agent names.
func herdrAgent(app string) string {
	switch app {
	case "claude-code":
		return "claude"
	}
	return app
}

// isAgent reports whether a program reported a status to herdr-core; plain
// shells never do.
func isAgent(t Terminal) bool {
	return t.Status != nil && (t.Status.App != "" || t.Status.State != "")
}

// agentStatus maps a herdr-core status to Herdr's idle/working/blocked/done/unknown.
func agentStatus(s *Status) string {
	if s == nil {
		return "unknown"
	}
	switch s.State {
	case "idle", "working", "blocked", "done":
		return s.State
	}
	return "unknown"
}

func (a *App) info(t Terminal, l Layout) paneInfo {
	p := paneInfo{
		PaneID:      t.Address,
		TerminalID:  t.Address,
		WorkspaceID: t.Labels["herdr.space"],
		TabID:       t.Labels["herdr.tab"],
		AgentStatus: agentStatus(t.Status),
		Revision:    t.Turn,
		Cwd:         t.Cwd,
		Title:       t.Title,
		Focused:     t.Address == l.Focused && l.Focused != "",
	}
	if ap, ok := l.Find(t.Address); ok {
		p.WorkspaceID, p.TabID = ap.Space, ap.Tab
	}
	if isAgent(t) {
		agent := t.Status.App
		if agent == "" {
			agent = t.Agent
		}
		if agent == "" && t.Foreground != nil {
			agent = t.Foreground.Name
		}
		if agent != "" {
			agent = herdrAgent(agent)
			p.Agent = &agent
		}
	}
	if t.Name != "" {
		name := t.Name
		p.Name = &name
	}
	return p
}

// layout is the window layout, or an empty one when the window is closed.
func (a *App) layout() Layout {
	l, err := a.Cmd.Layout()
	if err != nil {
		return Layout{}
	}
	return l
}

func (a *App) terminal(address string) (Terminal, error) {
	if address == "" {
		return Terminal{}, errf("invalid_params", "missing pane id")
	}
	if strings.Contains(address, "/") {
		return Terminal{}, errf("pane_not_found", "%s is on another machine; use `hc` for remote terminals", address)
	}
	t, err := a.Cmd.Get(address)
	if err != nil {
		return Terminal{}, errf("pane_not_found", "pane %s not found: %v", address, err)
	}
	return t, nil
}

func (a *App) list(agentsOnly bool, workspace string) ([]paneInfo, error) {
	terms, err := a.Cmd.Terminals()
	if err != nil {
		return nil, err
	}
	l := a.layout()
	out := []paneInfo{}
	for _, t := range terms {
		if t.State != "" && t.State != "live" {
			continue
		}
		if agentsOnly && !isAgent(t) {
			continue
		}
		p := a.info(t, l)
		if workspace != "" && p.WorkspaceID != workspace {
			continue
		}
		out = append(out, p)
	}
	return out, nil
}

// lastLines keeps the last n lines of text, without trailing blank lines.
func lastLines(text string, n int) (string, bool) {
	lines := strings.Split(strings.TrimRight(text, "\n "), "\n")
	if n <= 0 || len(lines) <= n {
		return strings.Join(lines, "\n"), false
	}
	return strings.Join(lines[len(lines)-n:], "\n"), true
}

type readParams struct {
	PaneID string `json:"pane_id"`
	Target string `json:"target"`
	Source string `json:"source"`
	Lines  *int   `json:"lines"`
}

func (a *App) read(address, source string, lines *int) (readResult, error) {
	t, err := a.terminal(address)
	if err != nil {
		return readResult{}, err
	}
	history := 0
	if source != "visible" {
		history = 1000
		if lines != nil {
			history = *lines
		}
	}
	text, err := a.Cmd.Read(t.Address, history)
	if err != nil {
		return readResult{}, err
	}
	truncated := false
	if lines != nil {
		text, truncated = lastLines(text, *lines)
	}
	if source == "" {
		source = "recent"
	}
	l := a.layout()
	p := a.info(t, l)
	return readResult{
		PaneID: t.Address, WorkspaceID: p.WorkspaceID, TabID: p.TabID,
		Source: source, Format: "text", Text: text, Revision: t.Turn, Truncated: truncated,
	}, nil
}

// hcKey turns a Herdr (tmux-style) key name into an hc one: Enter -> enter,
// C-c -> ctrl+c, C-M-x -> ctrl+alt+x, BTab -> shift+tab, PPage -> pageup.
func hcKey(k string) string {
	var mods []string
	for len(k) > 2 && k[1] == '-' {
		switch k[0] {
		case 'C', 'c':
			mods = append(mods, "ctrl")
		case 'M', 'm':
			mods = append(mods, "alt")
		case 'S', 's':
			mods = append(mods, "shift")
		default:
			return strings.Join(append(mods, hcKeyName(k)), "+")
		}
		k = k[2:]
	}
	return strings.Join(append(mods, hcKeyName(k)), "+")
}

func hcKeyName(k string) string {
	if len(k) == 1 {
		return k // a plain character keeps its case
	}
	switch lower := strings.ToLower(k); lower {
	case "return", "cr", "kpenter":
		return "enter"
	case "escape":
		return "esc"
	case "bspace", "bs":
		return "backspace"
	case "btab":
		return "shift+tab"
	case "ppage", "pgup":
		return "pageup"
	case "npage", "pgdn":
		return "pagedown"
	case "dc":
		return "delete"
	case "ic":
		return "insert"
	default:
		return lower
	}
}

func hcKeys(keys []string) []string {
	out := make([]string, len(keys))
	for i, k := range keys {
		out[i] = hcKey(k)
	}
	return out
}

// waitStatus polls a terminal until its agent status is one of until. A
// prompt passes the turn it started from: a status counts only once the turn
// moved on or the agent was seen working, so the reply to the prompt is
// waited for and not the state before it.
func (a *App) waitStatus(address string, until []string, timeout time.Duration, fromTurn *uint64, gone func() bool) (Terminal, error) {
	if len(until) == 0 {
		until = []string{"idle", "done", "blocked"}
	}
	var deadline time.Time
	if timeout > 0 {
		deadline = time.Now().Add(timeout)
	}
	started := fromTurn == nil
	for {
		t, err := a.terminal(address)
		if err != nil {
			return t, err
		}
		if !isAgent(t) && !slices.Contains(until, "unknown") {
			return t, errf("not_an_agent", "%s runs no agent that reports its status (hc get %s); use `herdr pane wait-output` or `hc wait`", t.Address, t.Address)
		}
		status := agentStatus(t.Status)
		if !started && (t.Turn != *fromTurn || status == "working") {
			started = true
		}
		if started && slices.Contains(until, status) {
			return t, nil
		}
		if t.State != "" && t.State != "live" {
			return t, errf("agent_exited", "%s is no longer running (%s)", t.Address, t.State)
		}
		if !deadline.IsZero() && time.Now().After(deadline) {
			return t, errf("timeout", "timed out waiting for %s to be %s (now %s)", t.Address, strings.Join(until, "/"), status)
		}
		if gone() {
			return t, errf("cancelled", "the client went away")
		}
		time.Sleep(a.poll())
	}
}

func (a *App) poll() time.Duration {
	if a.Poll > 0 {
		return a.Poll
	}
	return 300 * time.Millisecond
}

func millis(ms *uint64) time.Duration {
	if ms == nil {
		return 0
	}
	return time.Duration(*ms) * time.Millisecond
}

type outputMatch struct {
	Type  string `json:"type"`
	Value string `json:"value"`
}

func (a *App) waitOutput(address, source string, lines *int, match outputMatch, timeout time.Duration, gone func() bool) (map[string]any, error) {
	var re *regexp.Regexp
	switch match.Type {
	case "substring":
	case "regex":
		var err error
		if re, err = regexp.Compile(match.Value); err != nil {
			return nil, errf("invalid_params", "invalid regex: %v", err)
		}
	default:
		return nil, errf("invalid_params", "unknown match type %q", match.Type)
	}
	var deadline time.Time
	if timeout > 0 {
		deadline = time.Now().Add(timeout)
	}
	for {
		r, err := a.read(address, source, lines)
		if err != nil {
			return nil, err
		}
		for _, line := range strings.Split(r.Text, "\n") {
			if (re != nil && re.MatchString(line)) || (re == nil && strings.Contains(line, match.Value)) {
				return map[string]any{"type": "output_matched", "pane_id": r.PaneID, "revision": r.Revision, "read": r, "matched_line": line}, nil
			}
		}
		if !deadline.IsZero() && time.Now().After(deadline) {
			return nil, errf("timeout", "timed out waiting for output %q in %s", match.Value, r.PaneID)
		}
		if gone() {
			return nil, errf("cancelled", "the client went away")
		}
		time.Sleep(a.poll())
	}
}

// split opens a new terminal beside target in the app window and returns it.
func (a *App) split(target, direction, cwd string) (paneInfo, error) {
	if direction != "right" && direction != "down" {
		return paneInfo{}, errf("invalid_params", "direction must be right or down")
	}
	before, err := a.Cmd.Layout()
	if err != nil {
		return paneInfo{}, errf("unsupported", "pane split needs the Herdr app window open (%v); without it use `hc run --bg`", err)
	}
	if target == "" {
		target = before.Focused
	}
	pane, ok := before.Find(target)
	if !ok {
		return paneInfo{}, errf("pane_not_found", "%q has no pane in the app window; use `hc run --bg` for a terminal without one", target)
	}
	if cwd == "" {
		err = a.Cmd.AppDo("pane.split", map[string]any{"direction": direction, "pane": pane.Pane})
	} else if target == before.Focused {
		// Only terminal.open takes a folder, and it splits the focused pane.
		err = a.Cmd.AppDo("terminal.open", map[string]any{"place": direction, "folder": cwd})
	} else {
		return paneInfo{}, errf("unsupported", "the Herdr app only opens a split in a folder next to the focused pane; split %s without --cwd, or use `hc run --bg --cwd %s`", target, cwd)
	}
	if err != nil {
		return paneInfo{}, err
	}
	known := map[string]bool{}
	for _, p := range before.Panes {
		known[p.Terminal] = true
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		after, err := a.Cmd.Layout()
		if err == nil {
			// Prefer a new pane in the target's tab: another client may open
			// panes elsewhere at the same time.
			fresh := slices.DeleteFunc(slices.Clone(after.Panes), func(p AppPane) bool { return known[p.Terminal] })
			slices.SortStableFunc(fresh, func(x, y AppPane) int {
				return boolRank(x.Tab != pane.Tab) - boolRank(y.Tab != pane.Tab)
			})
			if len(fresh) > 0 {
				t, err := a.terminal(fresh[0].Terminal)
				if err != nil {
					return paneInfo{}, err
				}
				return a.info(t, after), nil
			}
		}
		if time.Now().After(deadline) {
			return paneInfo{}, errf("timeout", "the app did not show the new pane")
		}
		time.Sleep(a.poll())
	}
}

// close closes a terminal's pane in the app window, or ends a terminal that
// has no pane (a sub-agent).
func (a *App) close(address string) error {
	t, err := a.terminal(address)
	if err != nil {
		return err
	}
	if l, err := a.Cmd.Layout(); err == nil {
		if p, ok := l.Find(t.Address); ok {
			return a.Cmd.AppDo("pane.close", map[string]any{"pane": p.Pane})
		}
	}
	return a.Cmd.Kill(t.Address)
}

func boolRank(b bool) int {
	if b {
		return 1
	}
	return 0
}

var okResult = map[string]string{"type": "ok"}

// herdrAPI answers one request of the herdr CLI. Waits without a timeout end
// when gone reports that the client closed the connection.
func (a *App) herdrAPI(method string, raw json.RawMessage, gone func() bool) (any, error) {
	var p struct {
		PaneID       string        `json:"pane_id"`
		Target       string        `json:"target"`
		TargetPaneID string        `json:"target_pane_id"`
		CallerPaneID string        `json:"caller_pane_id"`
		WorkspaceID  string        `json:"workspace_id"`
		Source       string        `json:"source"`
		Lines        *int          `json:"lines"`
		Text         *string       `json:"text"`
		Keys         []string      `json:"keys"`
		Until        []string      `json:"until"`
		TimeoutMs    *uint64       `json:"timeout_ms"`
		Match        outputMatch   `json:"match"`
		Direction    string        `json:"direction"`
		Cwd          string        `json:"cwd"`
		Wait         *promptWaitOp `json:"wait"`
	}
	if len(raw) > 0 && string(raw) != "null" {
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, errf("invalid_params", "%v", err)
		}
	}
	target := p.PaneID
	if target == "" {
		target = p.Target
	}

	switch method {
	case "ping":
		version, protocol, err := a.Cmd.HerdrAPI()
		if err != nil {
			return nil, errf("internal", "herdr-focus-resume can't tell the herdr CLI's protocol: %v", err)
		}
		return map[string]any{"type": "pong", "version": version, "protocol": protocol}, nil

	case "pane.list", "agent.list":
		panes, err := a.list(method == "agent.list", p.WorkspaceID)
		if err != nil {
			return nil, err
		}
		if method == "agent.list" {
			return map[string]any{"type": "agent_list", "agents": panes}, nil
		}
		return map[string]any{"type": "pane_list", "panes": panes}, nil

	case "pane.get", "pane.current", "agent.get":
		if method == "pane.current" {
			target = p.CallerPaneID
			if target == "" {
				return nil, errf("invalid_params", "pane current needs HERDR_PANE_ID; in the app it is set from HC_TERMINAL_ID by `herdr-focus-resume app env`")
			}
		}
		t, err := a.terminal(target)
		if err != nil {
			return nil, err
		}
		info := a.info(t, a.layout())
		switch method {
		case "pane.current":
			return map[string]any{"type": "pane_current", "pane": info}, nil
		case "agent.get":
			return map[string]any{"type": "agent_info", "agent": info}, nil
		}
		return map[string]any{"type": "pane_info", "pane": info}, nil

	case "pane.read", "agent.read":
		r, err := a.read(target, p.Source, p.Lines)
		if err != nil {
			return nil, err
		}
		return map[string]any{"type": "pane_read", "read": r}, nil

	case "pane.send_keys", "agent.send_keys":
		t, err := a.terminal(target)
		if err != nil {
			return nil, err
		}
		if len(p.Keys) == 0 {
			return nil, errf("invalid_params", "no keys")
		}
		return okResult, a.Cmd.Keys(t.Address, hcKeys(p.Keys))

	case "pane.send_input":
		t, err := a.terminal(target)
		if err != nil {
			return nil, err
		}
		text := ""
		if p.Text != nil {
			text = *p.Text
		}
		keys := hcKeys(p.Keys)
		switch {
		case text == "":
			if len(keys) == 0 {
				return nil, errf("invalid_params", "no text or keys")
			}
			return okResult, a.Cmd.Keys(t.Address, keys)
		case len(keys) == 1 && keys[0] == "enter":
			return okResult, a.Cmd.Send(t.Address, text)
		}
		return nil, unsupported("pane.send_text")

	case "agent.prompt":
		if p.Text == nil || *p.Text == "" {
			return nil, errf("invalid_params", "empty prompt")
		}
		t, err := a.terminal(target)
		if err != nil {
			return nil, err
		}
		turn := t.Turn
		if err := a.Cmd.Send(t.Address, *p.Text); err != nil {
			return nil, err
		}
		if p.Wait != nil {
			if t, err = a.waitStatus(t.Address, p.Wait.Until, millis(p.Wait.TimeoutMs), &turn, gone); err != nil {
				return nil, err
			}
		} else if t, err = a.terminal(t.Address); err != nil {
			return nil, err
		}
		return map[string]any{"type": "agent_prompted", "agent": a.info(t, a.layout())}, nil

	case "agent.wait":
		t, err := a.waitStatus(target, p.Until, millis(p.TimeoutMs), nil, gone)
		if err != nil {
			return nil, err
		}
		return map[string]any{"type": "agent_info", "agent": a.info(t, a.layout())}, nil

	case "pane.wait_for_output":
		return a.waitOutput(target, p.Source, p.Lines, p.Match, millis(p.TimeoutMs), gone)

	case "pane.split":
		info, err := a.split(p.TargetPaneID, p.Direction, p.Cwd)
		if err != nil {
			return nil, err
		}
		return map[string]any{"type": "pane_info", "pane": info}, nil

	case "pane.close":
		return okResult, a.close(target)
	}
	return nil, unsupported(method)
}

type promptWaitOp struct {
	Until     []string `json:"until"`
	TimeoutMs *uint64  `json:"timeout_ms"`
}

// isWait reports whether method can block for longer than a normal request.
func isWait(method string) bool {
	switch method {
	case "agent.wait", "agent.prompt", "pane.wait_for_output", "pane.split", "pane.send_input":
		return true
	}
	return false
}

// replyErr writes err as a Herdr error reply.
func replyErr(conn net.Conn, id string, err error) {
	var ae *apiError
	if errors.As(err, &ae) {
		reply(conn, id, nil, ae.Code, ae.Message)
		return
	}
	reply(conn, id, nil, "internal", err.Error())
}
