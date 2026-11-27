package app

import (
	"bufio"
	"encoding/json"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// apiCore is a herdr-core with two app terminals in one tab (t7 a shell, t8
// a Claude Code agent) and a sub-agent t9 without a pane.
func apiCore() *fakeCore {
	core := newFakeCore("boot1")
	core.shell("t7", "/work")
	agent := core.shell("t8", "/work/api")
	agent.Status = &Status{App: "claude-code", State: "done"}
	agent.Turn = 3
	sub := core.shell("t9", "/work/api")
	sub.Status = &Status{App: "omp", State: "working"}
	sub.Name = "helper"
	core.panes = []AppPane{
		{Terminal: "t7", Pane: "p7", Tab: "tb1", Space: "sp1"},
		{Terminal: "t8", Pane: "p8", Tab: "tb1", Space: "sp1"},
	}
	core.focused = "t7"
	return core
}

type herdrClient struct {
	t    *testing.T
	conn net.Conn
	rd   *bufio.Reader
}

// serveAPI starts the daemon's socket and dials it like the herdr CLI does.
func serveAPI(t *testing.T, core *fakeCore) *herdrClient {
	t.Helper()
	a := newApp(t, core)
	a.Poll = 5 * time.Millisecond
	sock := filepath.Join(t.TempDir(), "h.sock")
	l, _, err := Listen(sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go a.Serve(l)
	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return &herdrClient{t: t, conn: conn, rd: bufio.NewReader(conn)}
}

type apiReply struct {
	ID     string          `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func (c *herdrClient) call(method, params string) apiReply {
	c.t.Helper()
	if params == "" {
		params = "{}"
	}
	c.conn.Write([]byte(`{"id":"cli:test","method":"` + method + `","params":` + params + "}\n"))
	c.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	b, err := c.rd.ReadBytes('\n')
	if err != nil {
		c.t.Fatalf("%s: %v", method, err)
	}
	var r apiReply
	if err := json.Unmarshal(b, &r); err != nil {
		c.t.Fatalf("%s: bad reply %s", method, b)
	}
	return r
}

// ok calls method and decodes its result into v, failing on an error reply.
func (c *herdrClient) ok(method, params string, v any) {
	c.t.Helper()
	r := c.call(method, params)
	if r.Error != nil {
		c.t.Fatalf("%s: error %s: %s", method, r.Error.Code, r.Error.Message)
	}
	if err := json.Unmarshal(r.Result, v); err != nil {
		c.t.Fatalf("%s: decode %s: %v", method, r.Result, err)
	}
}

// Regression: the app's shells point HERDR_SOCKET_PATH at the daemon, so the
// herdr CLI talks to it. It used to answer everything but integration reports
// with "unsupported", which broke `herdr pane list` and every other command.
func TestHerdrCLIPingAndPaneList(t *testing.T) {
	c := serveAPI(t, apiCore())

	var pong struct {
		Type     string `json:"type"`
		Protocol int    `json:"protocol"`
		Version  string `json:"version"`
	}
	c.ok("ping", "", &pong)
	if pong.Type != "pong" || pong.Protocol != 22 || pong.Version != "0.9.5" {
		t.Errorf("pong = %+v", pong)
	}

	var list struct {
		Type  string     `json:"type"`
		Panes []paneInfo `json:"panes"`
	}
	c.ok("pane.list", "", &list)
	if list.Type != "pane_list" || len(list.Panes) != 3 {
		t.Fatalf("pane list = %+v", list)
	}
	t7, t8 := list.Panes[0], list.Panes[1]
	if t7.PaneID != "t7" || !t7.Focused || t7.Agent != nil || t7.AgentStatus != "unknown" || t7.WorkspaceID != "sp1" || t7.TabID != "tb1" {
		t.Errorf("t7 = %+v", t7)
	}
	if t8.Agent == nil || *t8.Agent != "claude" || t8.AgentStatus != "done" || t8.Focused {
		t.Errorf("t8 = %+v", t8)
	}

	c.ok("pane.list", `{"workspace_id":"sp1"}`, &list)
	if len(list.Panes) != 2 {
		t.Errorf("workspace filter kept %d panes", len(list.Panes))
	}
}

func TestHerdrCLIPaneAndAgentLookups(t *testing.T) {
	c := serveAPI(t, apiCore())
	var cur struct {
		Type string   `json:"type"`
		Pane paneInfo `json:"pane"`
	}
	c.ok("pane.current", `{"caller_pane_id":"t8"}`, &cur)
	if cur.Type != "pane_current" || cur.Pane.PaneID != "t8" {
		t.Errorf("pane current = %+v", cur)
	}
	var agents struct {
		Agents []paneInfo `json:"agents"`
	}
	c.ok("agent.list", "", &agents)
	if len(agents.Agents) != 2 || agents.Agents[0].PaneID != "t8" || agents.Agents[1].PaneID != "t9" {
		t.Errorf("agent list = %+v", agents.Agents)
	}
	if n := agents.Agents[1].Name; n == nil || *n != "helper" {
		t.Errorf("t9 name = %v", n)
	}
	var get struct {
		Agent paneInfo `json:"agent"`
	}
	c.ok("agent.get", `{"target":"t9"}`, &get)
	if get.Agent.Agent == nil || *get.Agent.Agent != "omp" || get.Agent.AgentStatus != "working" {
		t.Errorf("agent get = %+v", get.Agent)
	}

	for _, tc := range []struct{ method, params, code string }{
		{"pane.get", `{"pane_id":"t42"}`, "pane_not_found"},
		{"pane.get", `{"pane_id":"vps/t1"}`, "pane_not_found"},
		{"pane.current", `{}`, "invalid_params"},
	} {
		if r := c.call(tc.method, tc.params); r.Error == nil || r.Error.Code != tc.code {
			t.Errorf("%s %s = %+v, want %s", tc.method, tc.params, r.Error, tc.code)
		}
	}
}

func TestHerdrCLIReadAndInput(t *testing.T) {
	core := apiCore()
	core.screens["t7"] = "one\ntwo\nthree\n\n"
	c := serveAPI(t, core)

	var read struct {
		Read readResult `json:"read"`
	}
	c.ok("pane.read", `{"pane_id":"t7","source":"recent","lines":2,"format":"text","strip_ansi":true}`, &read)
	if read.Read.Text != "two\nthree" || !read.Read.Truncated || read.Read.Source != "recent" {
		t.Errorf("read = %+v", read.Read)
	}

	var ok map[string]string
	c.ok("pane.send_input", `{"pane_id":"t7","text":"ls -la","keys":["Enter"]}`, &ok)
	if got := core.sent["t7"]; len(got) != 1 || got[0] != "ls -la" {
		t.Errorf("pane run sent %q", got)
	}
	c.ok("pane.send_keys", `{"pane_id":"t7","keys":["C-c","Escape","Up","q"]}`, &ok)
	if got := strings.Join(core.keys["t7"], " "); got != "ctrl+c esc up q" {
		t.Errorf("keys = %q", got)
	}

	// herdr-core can't paste without Enter: say what to use instead.
	r := c.call("pane.send_text", `{"pane_id":"t7","text":"hi"}`)
	if r.Error == nil || r.Error.Code != "unsupported" || !strings.Contains(r.Error.Message, "hc send") {
		t.Errorf("send_text = %+v", r.Error)
	}
	if len(core.sent["t7"]) != 1 {
		t.Errorf("send_text typed something")
	}
}

func TestHerdrCLIUnsupportedNamesTheAlternative(t *testing.T) {
	c := serveAPI(t, apiCore())
	for method, want := range map[string]string{
		"workspace.create": "herdr-app do space.new",
		"agent.start":      "hc run --bg",
		"tab.list":         "herdr-app",
	} {
		r := c.call(method, "")
		if r.Error == nil || r.Error.Code != "unsupported" || !strings.Contains(r.Error.Message, want) {
			t.Errorf("%s = %+v, want a hint with %q", method, r.Error, want)
		}
	}
}

func TestHerdrCLIAgentWait(t *testing.T) {
	core := apiCore()
	c := serveAPI(t, core)

	// Default states are idle, done and blocked: t8 is done already.
	var info struct {
		Agent paneInfo `json:"agent"`
	}
	c.ok("agent.wait", `{"target":"t8"}`, &info)
	if info.Agent.AgentStatus != "done" {
		t.Errorf("wait = %+v", info.Agent)
	}

	go func() {
		time.Sleep(30 * time.Millisecond)
		core.mu.Lock()
		core.terms["t9"].Status.State = "blocked"
		core.mu.Unlock()
	}()
	c.ok("agent.wait", `{"target":"t9","until":["blocked"]}`, &info)
	if info.Agent.AgentStatus != "blocked" {
		t.Errorf("wait blocked = %+v", info.Agent)
	}

	r := c.call("agent.wait", `{"target":"t9","until":["idle"],"timeout_ms":30}`)
	if r.Error == nil || r.Error.Code != "timeout" {
		t.Errorf("timeout = %+v", r.Error)
	}
}

// A prompt with wait must wait for the reply to the prompt, not return on the
// "done" the agent was in before it.
func TestHerdrCLIAgentPromptWaitsForItsTurn(t *testing.T) {
	core := apiCore()
	core.onSend = func(addr string) {
		go func() {
			time.Sleep(30 * time.Millisecond)
			core.mu.Lock()
			core.terms[addr].Status.State = "working"
			core.mu.Unlock()
			time.Sleep(30 * time.Millisecond)
			core.mu.Lock()
			core.terms[addr].Status.State = "done"
			core.terms[addr].Turn++
			core.mu.Unlock()
		}()
	}
	c := serveAPI(t, core)
	var res struct {
		Type  string   `json:"type"`
		Agent paneInfo `json:"agent"`
	}
	c.ok("agent.prompt", `{"target":"t8","text":"fix it","wait":{"until":["done"]}}`, &res)
	if res.Type != "agent_prompted" || res.Agent.Revision != 4 {
		t.Errorf("prompt returned before its turn: %+v", res)
	}
	if got := core.sent["t8"]; len(got) != 1 || got[0] != "fix it" {
		t.Errorf("sent %q", got)
	}
}

func TestHerdrCLIWaitForOutput(t *testing.T) {
	core := apiCore()
	core.screens["t7"] = "building\n"
	go func() {
		time.Sleep(30 * time.Millisecond)
		core.mu.Lock()
		core.screens["t7"] = "building\nPASS ok 12 tests\n"
		core.mu.Unlock()
	}()
	c := serveAPI(t, core)
	var res struct {
		Type        string `json:"type"`
		MatchedLine string `json:"matched_line"`
	}
	c.ok("pane.wait_for_output", `{"pane_id":"t7","source":"recent","match":{"type":"regex","value":"ok \\d+ tests"}}`, &res)
	if res.Type != "output_matched" || res.MatchedLine != "PASS ok 12 tests" {
		t.Errorf("wait output = %+v", res)
	}
	r := c.call("pane.wait_for_output", `{"pane_id":"t7","source":"recent","match":{"type":"substring","value":"nope"},"timeout_ms":20}`)
	if r.Error == nil || r.Error.Code != "timeout" {
		t.Errorf("timeout = %+v", r.Error)
	}
}

func TestHerdrCLISplitAndClose(t *testing.T) {
	tests := []struct {
		name, params string
		did          string // herdr-app do command, "" when none
		code         string // error code, "" for success
		closed       bool   // the app window is closed
		race         bool   // another pane appears in another tab meanwhile
	}{
		{name: "pane opened elsewhere meanwhile", params: `{"target_pane_id":"t8","direction":"right"}`, did: `pane.split {"direction":"right","pane":"p8"}`, race: true},
		{name: "split target", params: `{"target_pane_id":"t8","direction":"right"}`, did: `pane.split {"direction":"right","pane":"p8"}`},
		{name: "split focused", params: `{"direction":"down"}`, did: `pane.split {"direction":"down","pane":"p7"}`},
		{name: "cwd next to focused", params: `{"target_pane_id":"t7","direction":"down","cwd":"/srv"}`, did: `terminal.open {"folder":"/srv","place":"down"}`},
		{name: "cwd elsewhere", params: `{"target_pane_id":"t8","direction":"down","cwd":"/srv"}`, code: "unsupported"},
		{name: "sub-agent has no pane", params: `{"target_pane_id":"t9","direction":"down"}`, code: "pane_not_found"},
		{name: "bad direction", params: `{"target_pane_id":"t8","direction":"up"}`, code: "invalid_params"},
		{name: "window closed", params: `{"direction":"down"}`, code: "unsupported", closed: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			core := apiCore()
			if tc.closed {
				core.panes = nil
			}
			core.racePane = tc.race
			c := serveAPI(t, core)
			r := c.call("pane.split", tc.params)
			if tc.code != "" {
				if r.Error == nil || r.Error.Code != tc.code {
					t.Fatalf("error = %+v, want %s", r.Error, tc.code)
				}
				if len(core.did) != 0 {
					t.Errorf("ran %v", core.did)
				}
				return
			}
			if r.Error != nil {
				t.Fatalf("error %+v", r.Error)
			}
			var res struct {
				Pane paneInfo `json:"pane"`
			}
			json.Unmarshal(r.Result, &res)
			if res.Pane.PaneID != "t99" || res.Pane.TabID != "tb1" {
				t.Errorf("new pane = %+v", res.Pane)
			}
			if len(core.did) != 1 || core.did[0] != tc.did {
				t.Errorf("did %v, want %s", core.did, tc.did)
			}
		})
	}

	core := apiCore()
	c := serveAPI(t, core)
	var ok map[string]string
	c.ok("pane.close", `{"pane_id":"t8"}`, &ok)
	c.ok("pane.close", `{"pane_id":"t9"}`, &ok)
	if len(core.did) != 1 || core.did[0] != `pane.close {"pane":"p8"}` {
		t.Errorf("close did %v", core.did)
	}
	if len(core.killed) != 1 || core.killed[0] != "t9" {
		t.Errorf("killed %v, want the pane-less t9", core.killed)
	}
}

func TestStatusDecodesLsAndGet(t *testing.T) {
	for _, tc := range []struct{ in, app, state string }{
		{`{"status":"working"}`, "", "working"},
		{`{"status":{"app":"omp","state":"blocked"}}`, "omp", "blocked"},
	} {
		var term Terminal
		if err := json.Unmarshal([]byte(tc.in), &term); err != nil {
			t.Fatalf("%s: %v", tc.in, err)
		}
		if term.Status == nil || term.Status.App != tc.app || term.Status.State != tc.state {
			t.Errorf("%s: %+v", tc.in, term.Status)
		}
	}
	var none Terminal
	json.Unmarshal([]byte(`{"status":null}`), &none)
	if none.Status != nil {
		t.Errorf("null status = %+v", none.Status)
	}
}

func FuzzLastLines(f *testing.F) {
	f.Add("a\nb\nc\n\n", 2)
	f.Add("", 0)
	f.Add("\n\n\n", 1)
	f.Fuzz(func(t *testing.T, text string, n int) {
		got, truncated := lastLines(text, n)
		lines := strings.Count(got, "\n") + 1
		if n > 0 && lines > n {
			t.Fatalf("lastLines(%q, %d) kept %d lines", text, n, lines)
		}
		if !strings.Contains(text, got) {
			t.Fatalf("lastLines(%q, %d) = %q, not part of the input", text, n, got)
		}
		if truncated && n <= 0 {
			t.Fatalf("truncated without a limit")
		}
	})
}

// `herdr agent wait` without --timeout waits forever; when the person stops
// it (Ctrl-C closes the socket), the daemon must stop polling hc.
func TestHerdrCLIWaitStopsWhenClientLeaves(t *testing.T) {
	core := apiCore()
	c := serveAPI(t, core)
	c.conn.Write([]byte(`{"id":"cli:agent:wait","method":"agent.wait","params":{"target":"t9","until":["idle"]}}` + "\n"))
	time.Sleep(30 * time.Millisecond)
	c.conn.Close()
	time.Sleep(50 * time.Millisecond)
	core.mu.Lock()
	before := core.gets
	core.mu.Unlock()
	time.Sleep(50 * time.Millisecond)
	core.mu.Lock()
	after := core.gets
	core.mu.Unlock()
	if after != before {
		t.Errorf("still polling after the client left: %d -> %d Get calls", before, after)
	}
}

func TestHCKey(t *testing.T) {
	for in, want := range map[string]string{
		"Enter": "enter", "C-c": "ctrl+c", "M-x": "alt+x", "C-M-x": "ctrl+alt+x",
		"S-Tab": "shift+tab", "BTab": "shift+tab", "PPage": "pageup", "NPage": "pagedown",
		"Escape": "esc", "BSpace": "backspace", "Up": "up", "q": "q", "Q": "Q", "-": "-", "F5": "f5",
	} {
		if got := hcKey(in); got != want {
			t.Errorf("hcKey(%q) = %q, want %q", in, got, want)
		}
	}
}

// A plain shell never reports a status: waiting for it to be idle would
// block forever, so say so instead.
func TestHerdrCLIAgentWaitOnShell(t *testing.T) {
	c := serveAPI(t, apiCore())
	r := c.call("agent.wait", `{"target":"t7"}`)
	if r.Error == nil || r.Error.Code != "not_an_agent" {
		t.Errorf("wait on a shell = %+v", r.Error)
	}
	var info struct {
		Agent paneInfo `json:"agent"`
	}
	c.ok("agent.wait", `{"target":"t7","until":["unknown"]}`, &info)
	if info.Agent.AgentStatus != "unknown" {
		t.Errorf("until unknown = %+v", info.Agent)
	}
}
