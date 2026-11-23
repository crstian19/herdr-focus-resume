package lazy

import (
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"
)

type fakeHerdr struct {
	mu    sync.Mutex
	panes []Pane
	busy  map[string]int // pane -> number of ShellIdle calls that report busy
	ran   []string
}

func (f *fakeHerdr) ListPanes() ([]Pane, error) { return f.panes, nil }

func (f *fakeHerdr) ShellIdle(id string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.busy[id] > 0 {
		f.busy[id]--
		return false, nil
	}
	return f.busy[id] == 0, nil
}

func (f *fakeHerdr) Run(id, line string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ran = append(f.ran, id+" "+line)
	return nil
}

func newRunner(t *testing.T, h *fakeHerdr) *Runner {
	t.Helper()
	return &Runner{
		Herdr:      h,
		LedgerPath: filepath.Join(t.TempDir(), "ledger.json"),
		IdleWait:   time.Second,
		Sleep:      func(time.Duration) {},
		Log:        t.Logf,
	}
}

var dormantFixture = []Dormant{
	{PaneID: "w1:p1", Cwd: "/a", Agent: "claude", Argv: []string{"claude", "--resume", "A"}},
	{PaneID: "w1:p2", Cwd: "/b", Agent: "claude", Argv: []string{"claude", "--resume", "B"}},
	{PaneID: "w2:p1", Cwd: "/c", Agent: "omp", Argv: []string{"omp", "--resume=/s"}},
	{PaneID: "w9:p9", Cwd: "/gone", Agent: "claude", Argv: []string{"claude", "--resume", "Z"}},
}

func TestStartupWakesOnlyFocusedPane(t *testing.T) {
	h := &fakeHerdr{panes: []Pane{
		{ID: "w1:p1", Cwd: "/a"},
		{ID: "w1:p2", Cwd: "/b", Focused: true},
		{ID: "w2:p1", Cwd: "/c"},
	}}
	r := newRunner(t, h)
	if err := r.Startup(dormantFixture); err != nil {
		t.Fatal(err)
	}
	want := []string{"w1:p2 'claude' '--resume' 'B'"}
	if !reflect.DeepEqual(h.ran, want) {
		t.Fatalf("ran %q, want %q", h.ran, want)
	}
}

func TestFocusWakesOncePerPane(t *testing.T) {
	h := &fakeHerdr{panes: []Pane{{ID: "w1:p1", Cwd: "/a"}, {ID: "w2:p1", Cwd: "/c"}}}
	r := newRunner(t, h)
	if err := r.Startup(dormantFixture); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"w2:p1", "w2:p1", "w1:p1", "w3:p1"} {
		if err := r.Focus(id); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{"w2:p1 'omp' '--resume=/s'", "w1:p1 'claude' '--resume' 'A'"}
	if !reflect.DeepEqual(h.ran, want) {
		t.Fatalf("ran %q, want %q", h.ran, want)
	}
}

func TestStartupSkipsPanesWhoseCwdMoved(t *testing.T) {
	h := &fakeHerdr{panes: []Pane{{ID: "w1:p1", Cwd: "/elsewhere"}}}
	r := newRunner(t, h)
	if err := r.Startup(dormantFixture); err != nil {
		t.Fatal(err)
	}
	if err := r.Focus("w1:p1"); err != nil {
		t.Fatal(err)
	}
	if len(h.ran) != 0 {
		t.Fatalf("resumed into a pane that no longer matches: %q", h.ran)
	}
}

func TestFocusWaitsForShellThenDropsBusyPane(t *testing.T) {
	h := &fakeHerdr{
		panes: []Pane{{ID: "w1:p1", Cwd: "/a"}, {ID: "w1:p2", Cwd: "/b"}},
		busy:  map[string]int{"w1:p1": 2, "w1:p2": 1 << 30},
	}
	r := newRunner(t, h)
	if err := r.Startup(dormantFixture); err != nil {
		t.Fatal(err)
	}
	_ = r.Focus("w1:p1") // shell settles after two polls: resumed
	_ = r.Focus("w1:p2") // never idle: user owns the pane, dropped
	h.busy["w1:p2"] = 0
	_ = r.Focus("w1:p2") // must stay dropped
	want := []string{"w1:p1 'claude' '--resume' 'A'"}
	if !reflect.DeepEqual(h.ran, want) {
		t.Fatalf("ran %q, want %q", h.ran, want)
	}
}

func TestConcurrentFocusResumesOnce(t *testing.T) {
	h := &fakeHerdr{panes: []Pane{{ID: "w1:p1", Cwd: "/a"}}}
	r := newRunner(t, h)
	if err := r.Startup(dormantFixture); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Separate Runner values share only the ledger file, like
			// separate hook processes do.
			rr := *r
			if err := rr.Focus("w1:p1"); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if len(h.ran) != 1 {
		t.Fatalf("resumed %d times, want 1: %q", len(h.ran), h.ran)
	}
}
