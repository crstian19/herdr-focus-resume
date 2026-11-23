package lazy

import (
	"errors"
	"os/exec"
	"reflect"
	"strings"
	"testing"
)

func TestResumeArgv(t *testing.T) {
	cases := []struct {
		name    string
		resume  *AgentResume
		session *AgentSession
		want    []string
		err     error
	}{
		{
			name:    "claude native session",
			session: &AgentSession{Agent: "claude", Kind: "id", Value: "abc-123"},
			want:    []string{"claude", "--resume", "abc-123"},
		},
		{
			name:    "omp session path uses = form",
			session: &AgentSession{Agent: "omp", Kind: "path", Value: "/home/u/.omp/s.jsonl"},
			want:    []string{"omp", "--resume=/home/u/.omp/s.jsonl"},
		},
		{
			name:    "codex uses subcommand",
			session: &AgentSession{Agent: "codex", Value: "x"},
			want:    []string{"codex", "resume", "x"},
		},
		{
			name:    "reported resume command wins over native table",
			resume:  &AgentResume{Agent: "mine", Argv: []string{"mine", "--resume", "s1"}},
			session: &AgentSession{Agent: "claude", Value: "other"},
			want:    []string{"mine", "--resume", "s1"},
		},
		{name: "nothing to resume", err: ErrNoResume},
		{
			name:    "unknown agent",
			session: &AgentSession{Agent: "mystery", Value: "x"},
			err:     ErrUnknownAgent,
		},
		{
			name:   "path as command is rejected",
			resume: &AgentResume{Argv: []string{"/usr/bin/evil"}},
			err:    ErrInvalidResume,
		},
		{
			name:    "control character in session value is rejected",
			session: &AgentSession{Agent: "claude", Value: "x\n rm -rf ~"},
			err:     ErrInvalidResume,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := ResumeArgv(c.resume, c.session)
			if !errors.Is(err, c.err) {
				t.Fatalf("err = %v, want %v", err, c.err)
			}
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("argv = %q, want %q", got, c.want)
			}
		})
	}
}

// The pane's shell must parse ShellQuote's output back into the same argv,
// whatever the arguments contain.
func FuzzShellQuoteRoundTrip(f *testing.F) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		f.Skip("no sh")
	}
	f.Add("claude", "--resume", "abc")
	f.Add("x", "it's", "$(touch /tmp/pwned)")
	f.Add("x", "`id`; echo \"hi\"", "a b\tc")
	f.Add("x", "'''", "\\'")
	f.Fuzz(func(t *testing.T, a, b, c string) {
		argv := []string{a, b, c}
		for _, s := range argv {
			if strings.ContainsRune(s, 0) {
				t.Skip("NUL cannot cross argv")
			}
		}
		script := `for a in "$@"; do printf '%s\0' "$a"; done`
		out, err := exec.Command(sh, "-c", "set -- "+ShellQuote(argv)+"; "+script).Output()
		if err != nil {
			t.Fatalf("sh failed for %q: %v", argv, err)
		}
		got := strings.Split(strings.TrimSuffix(string(out), "\x00"), "\x00")
		if !reflect.DeepEqual(got, argv) {
			t.Fatalf("round trip %q -> %q", argv, got)
		}
	})
}
