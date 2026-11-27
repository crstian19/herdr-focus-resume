package app

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"path/filepath"
	"sort"
	"sync"
	"testing"
	"time"
)

// fakeCore is an in-memory herdr-core: terminals, labels, the holder instance
// and the app's focused terminal.
type fakeCore struct {
	mu       sync.Mutex
	terms    map[string]*Terminal
	instance string
	focused  string
	sent     map[string][]string

	// Herdr API side.
	screens map[string]string   // hc read text per terminal
	keys    map[string][]string // keys pressed per terminal
	killed  []string
	panes   []AppPane // app window layout; nil means the window is closed
	did     []string  // herdr-app do commands with their JSON args
	// onSend runs after Send, for tests that move an agent through states.
	onSend   func(addr string)
	gets     int  // Get calls, to see a wait stop polling
	racePane bool // AppDo split also shows another new pane in tab tb2
}

func newFakeCore(instance string) *fakeCore {
	return &fakeCore{terms: map[string]*Terminal{}, instance: instance, sent: map[string][]string{},
		screens: map[string]string{}, keys: map[string][]string{}}
}

// shell adds a terminal whose foreground process is its shell.
func (f *fakeCore) shell(addr, cwd string) *Terminal {
	t := &Terminal{Address: addr, Cwd: cwd, State: "live", Pid: 100, Labels: map[string]string{}}
	t.Foreground = &struct {
		Name string `json:"name"`
		Pid  int    `json:"pid"`
	}{Name: "zsh", Pid: 100}
	f.terms[addr] = t
	return t
}

func (f *fakeCore) Terminals() ([]Terminal, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []Terminal
	for _, t := range f.terms {
		lt := Terminal{Address: t.Address, Name: t.Name, Cwd: t.Cwd, State: t.State, Turn: t.Turn}
		lt.Agent = "zsh"
		if t.Status != nil {
			// hc ls has the state, and the app as the terminal's agent.
			lt.Status = &Status{State: t.Status.State}
			lt.Agent = t.Status.App
		}
		out = append(out, lt)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Address < out[j].Address })
	return out, nil
}

func (f *fakeCore) Get(addr string) (Terminal, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gets++
	ft, ok := f.terms[addr]
	if !ok {
		return Terminal{}, fmt.Errorf("no terminal %s", addr)
	}
	t := *ft
	if t.Status != nil {
		st := *t.Status
		t.Status = &st
	}
	labels := map[string]string{}
	for k, v := range t.Labels {
		labels[k] = v
	}
	t.Labels = labels
	return t, nil
}

func (f *fakeCore) Label(addr string, labels map[string]string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for k, v := range labels {
		if v == "" {
			delete(f.terms[addr].Labels, k)
		} else {
			f.terms[addr].Labels[k] = v
		}
	}
	return nil
}

func (f *fakeCore) Send(addr, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent[addr] = append(f.sent[addr], text)
	hook := f.onSend
	f.mu.Unlock()
	if hook != nil {
		hook(addr)
	}
	f.mu.Lock()
	return nil
}

func (f *fakeCore) Instance() (string, error)        { return f.instance, nil }
func (f *fakeCore) FocusedTerminal() (string, error) { return f.focused, nil }

func newApp(t *testing.T, core *fakeCore) *App {
	t.Helper()
	return &App{Cmd: core, StateDir: t.TempDir(), Log: t.Logf}
}

func TestRecordStoresSessionOnTerminal(t *testing.T) {
	core := newFakeCore("boot1")
	core.shell("t3", "/work/api")
	a := newApp(t, core)

	if err := a.Record(Report{PaneID: "t3", Agent: "claude", SessionID: "abc-123"}); err != nil {
		t.Fatal(err)
	}
	l := core.terms["t3"].Labels
	want := map[string]string{LabelAgent: "claude", LabelSession: "abc-123", LabelCwd: "/work/api", LabelInstance: "boot1"}
	for k, v := range want {
		if l[k] != v {
			t.Errorf("label %s = %q, want %q", k, l[k], v)
		}
	}
	if _, ok := l[LabelArgv]; ok {
		t.Errorf("argv label set without a reported resume command")
	}
}

func TestRecordUsesSessionPathAndIgnoresUnknownAgents(t *testing.T) {
	core := newFakeCore("boot1")
	core.shell("t4", "/work")
	core.shell("t5", "/work")
	a := newApp(t, core)

	a.Record(Report{PaneID: "t4", Agent: "omp", SessionPath: "/home/u/.omp/s.jsonl"})
	if got := core.terms["t4"].Labels[LabelSession]; got != "/home/u/.omp/s.jsonl" {
		t.Errorf("omp session = %q", got)
	}
	a.Record(Report{PaneID: "t5", Agent: "not-an-agent", SessionID: "x"})
	if len(core.terms["t5"].Labels) != 0 {
		t.Errorf("unknown agent recorded: %v", core.terms["t5"].Labels)
	}
	if err := a.Record(Report{PaneID: "vps/t1", Agent: "claude", SessionID: "x"}); err != nil {
		t.Errorf("remote terminal: %v", err)
	}
}

// After a lost holder (new instance) the recorded terminals are dormant: the
// focused one resumes at once, the others when focused, each only once.
func TestPrepareAndFocusResumeOncePerInstance(t *testing.T) {
	core := newFakeCore("boot1")
	core.shell("t2", "/work/api")
	core.shell("t3", "/work/web")
	core.shell("t4", "/work/new")
	a := newApp(t, core)
	a.Record(Report{PaneID: "t2", Agent: "claude", SessionID: "s-api"})
	a.Record(Report{PaneID: "t3", Agent: "codex", SessionID: "s-web"})

	// Reboot: every terminal came back as a shell under a new holder.
	core.instance = "boot2"
	core.focused = "t3"
	if _, err := a.Prepare(); err != nil {
		t.Fatal(err)
	}
	if got := core.sent["t3"]; len(got) != 1 || got[0] != "codex 'resume' 's-web'" {
		t.Fatalf("focused t3 sent %q", got)
	}
	if len(core.sent["t2"]) != 0 {
		t.Fatalf("t2 resumed before focus")
	}

	a.Focus("t2")
	a.Focus("t2")
	a.Focus("t4")
	if got := core.sent["t2"]; len(got) != 1 || got[0] != "claude '--resume' 's-api'" {
		t.Errorf("t2 sent %q, want one resume", got)
	}
	if len(core.sent["t4"]) != 0 {
		t.Errorf("t4 had no recorded session but got %q", core.sent["t4"])
	}
	// A second Prepare in the same instance changes nothing.
	a.Prepare()
	if len(core.sent["t3"]) != 1 {
		t.Errorf("t3 resumed twice")
	}
}

// Sessions recorded in the current instance are live agents, never dormant.
func TestCurrentInstanceIsNotDormant(t *testing.T) {
	core := newFakeCore("boot1")
	core.shell("t2", "/work")
	a := newApp(t, core)
	a.Record(Report{PaneID: "t2", Agent: "claude", SessionID: "s1"})
	core.focused = "t2"
	a.Prepare()
	a.Focus("t2")
	if len(core.sent["t2"]) != 0 {
		t.Errorf("resumed a session from this boot: %q", core.sent["t2"])
	}
}

func TestDormantSkipsMovedOrBusyTerminals(t *testing.T) {
	core := newFakeCore("boot1")
	core.shell("t2", "/work/api")
	busy := core.shell("t3", "/work/web")
	a := newApp(t, core)
	a.IdleWait = 10 * time.Millisecond
	a.Record(Report{PaneID: "t2", Agent: "claude", SessionID: "s-api"})
	a.Record(Report{PaneID: "t3", Agent: "claude", SessionID: "s-web"})

	core.instance = "boot2"
	core.terms["t2"].Cwd = "/elsewhere" // the shell came back in another folder
	busy.Foreground.Pid = 999           // something else runs in t3
	a.Prepare()
	a.Focus("t2")
	a.Focus("t3")
	if len(core.sent) != 0 {
		t.Errorf("sent %v", core.sent)
	}
}

func TestReportedResumeCommandWins(t *testing.T) {
	core := newFakeCore("boot1")
	core.shell("t2", "/work")
	a := newApp(t, core)
	var r Report
	json.Unmarshal([]byte(`{"pane_id":"t2","agent":"myagent","agent_session_id":"z",
		"agent_resume":{"agent":"myagent","argv":["myagent","--continue","z"]}}`), &r)
	a.Record(r)
	core.instance = "boot2"
	core.focused = "t2"
	a.Prepare()
	if got := core.sent["t2"]; len(got) != 1 || got[0] != "myagent '--continue' 'z'" {
		t.Errorf("sent %q", got)
	}
}

// The socket speaks the protocol of Herdr's integrations: one JSON request per
// line, one JSON reply per line.
func TestServeRecordsIntegrationReports(t *testing.T) {
	core := newFakeCore("boot1")
	core.shell("t7", "/work")
	a := newApp(t, core)
	sock := filepath.Join(t.TempDir(), "h.sock")
	l, running, err := Listen(sock)
	if err != nil || running {
		t.Fatalf("listen: %v running=%v", err, running)
	}
	defer l.Close()
	go a.Serve(l)

	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	rd := bufio.NewReader(conn)
	send := func(line string) map[string]any {
		conn.Write([]byte(line + "\n"))
		b, err := rd.ReadBytes('\n')
		if err != nil {
			t.Fatal(err)
		}
		var resp map[string]any
		json.Unmarshal(b, &resp)
		return resp
	}

	resp := send(`{"id":"r1","method":"pane.report_agent_session","params":{"pane_id":"t7","source":"herdr:claude","agent":"claude","seq":1,"agent_session_id":"sess-7"}}`)
	if resp["id"] != "r1" || resp["result"] == nil {
		t.Errorf("reply %v", resp)
	}
	if resp := send(`{"id":"r2","method":"pane.report_agent","params":{"pane_id":"t7","state":"working"}}`); resp["result"] == nil {
		t.Errorf("report_agent reply %v", resp)
	}
	if resp := send(`{"id":"r3","method":"workspace.create","params":{}}`); resp["error"] == nil {
		t.Errorf("unsupported method answered %v", resp)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if core.terms["t7"].Labels[LabelSession] == "sess-7" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := core.terms["t7"].Labels[LabelSession]; got != "sess-7" {
		t.Errorf("session label %q", got)
	}
	if !Alive(sock) {
		t.Errorf("Alive = false while serving")
	}
	if _, running, _ := Listen(sock); !running {
		t.Errorf("second Listen did not see the running daemon")
	}
}

func (f *fakeCore) Read(addr string, lines int) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.screens[addr], nil
}

func (f *fakeCore) Keys(addr string, keys []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.keys[addr] = append(f.keys[addr], keys...)
	return nil
}

func (f *fakeCore) Kill(addr string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.killed = append(f.killed, addr)
	return nil
}

func (f *fakeCore) Layout() (Layout, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.panes == nil {
		return Layout{}, fmt.Errorf("herdr-app: window not open")
	}
	return Layout{Panes: append([]AppPane(nil), f.panes...), Focused: f.focused}, nil
}

// AppDo records the command; pane.split and terminal.open add a shell t99
// in the target's tab, pane.close removes the pane.
func (f *fakeCore) AppDo(command string, args map[string]any) error {
	b, _ := json.Marshal(args)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.did = append(f.did, command+" "+string(b))
	switch command {
	case "pane.split", "terminal.open":
		if f.racePane {
			// Someone opens a tab elsewhere at the same moment.
			f.panes = append(f.panes, AppPane{Terminal: "t98", Pane: "p98", Tab: "tb2", Space: "sp1"})
			f.terms["t98"] = &Terminal{Address: "t98", State: "live", Labels: map[string]string{}}
		}
		tab := f.panes[0]
		f.panes = append(f.panes, AppPane{Terminal: "t99", Pane: "p99", Tab: tab.Tab, Space: tab.Space})
		f.terms["t99"] = &Terminal{Address: "t99", Cwd: "/new", State: "live", Labels: map[string]string{}}
	case "pane.close":
		var kept []AppPane
		for _, p := range f.panes {
			if p.Pane != args["pane"] {
				kept = append(kept, p)
			}
		}
		f.panes = kept
	}
	return nil
}

func (f *fakeCore) HerdrAPI() (string, int, error) { return "0.9.5", 22, nil }
