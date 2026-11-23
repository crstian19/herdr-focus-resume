package wake

import (
	"os"
	"sort"
	"strings"
	"testing"
)

func TestParseSession(t *testing.T) {
	f, err := os.Open("testdata/session.json")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	dormant, skipped, err := ParseSession(f)
	if err != nil {
		t.Fatal(err)
	}
	sort.Slice(dormant, func(i, j int) bool { return dormant[i].PaneID < dormant[j].PaneID })

	want := map[string]string{
		"w8:p1E": "'claude' '--resume' 'sess-claude'",
		"w8:p1W": "'omp' '--resume=/tmp/omp/s.jsonl'",
		"wB:pG":  "'mine' '--resume' 'R1'",
	}
	if len(dormant) != len(want) {
		t.Fatalf("got %d dormant panes %+v, want %d", len(dormant), dormant, len(want))
	}
	for _, d := range dormant {
		if got := ShellQuote(d.Argv); got != want[d.PaneID] {
			t.Errorf("%s: %s, want %s", d.PaneID, got, want[d.PaneID])
		}
	}
	if len(skipped) != 1 || skipped[0].PaneID != "wB:pH" {
		t.Fatalf("skipped = %+v, want only wB:pH (unknown agent)", skipped)
	}
}

func TestResumeOnRestoreDisabled(t *testing.T) {
	cases := []struct {
		name string
		toml string
		want bool
	}{
		{"missing key keeps herdr default", "onboarding = false\n", false},
		{"disabled in section", "[session]\nresume_agents_on_restore = false\n", true},
		{"enabled in section", "[session]\nresume_agents_on_restore = true\n", false},
		{"dotted top-level key", "session.resume_agents_on_restore = false\n", true},
		{"key in another section ignored", "[ui]\nresume_agents_on_restore = false\n", false},
		{"commented out", "[session]\n# resume_agents_on_restore = false\n", false},
		{"trailing comment", "[session]\nresume_agents_on_restore = false # lazy\n", true},
		{"later section does not leak", "[session]\n[ui]\nresume_agents_on_restore = false\n", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ResumeOnRestoreDisabled(strings.NewReader(c.toml)); got != c.want {
				t.Fatalf("got %v, want %v", got, c.want)
			}
		})
	}
}
