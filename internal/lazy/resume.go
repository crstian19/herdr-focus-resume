package lazy

import (
	"errors"
	"strings"
	"unicode"
)

// AgentSession is the native session reference Herdr's own integrations
// store per pane (session.json "agent_session").
type AgentSession struct {
	Agent  string `json:"agent"`
	Kind   string `json:"kind"`
	Source string `json:"source"`
	Value  string `json:"value"`
}

// AgentResume is the resume command a third-party integration reported
// (session.json "agent_resume").
type AgentResume struct {
	Agent  string   `json:"agent"`
	Argv   []string `json:"argv"`
	Source string   `json:"source"`
}

// nativeResume maps Herdr's built-in agent names to their resume command, as
// documented in Herdr's "Session state and restore" page (v0.9.5).
var nativeResume = map[string]func(v string) []string{
	"claude":     func(v string) []string { return []string{"claude", "--resume", v} },
	"codex":      func(v string) []string { return []string{"codex", "resume", v} },
	"omp":        func(v string) []string { return []string{"omp", "--resume=" + v} },
	"pi":         func(v string) []string { return []string{"pi", "--session", v} },
	"opencode":   func(v string) []string { return []string{"opencode", "--session", v} },
	"kilo":       func(v string) []string { return []string{"kilo", "--session", v} },
	"kimi":       func(v string) []string { return []string{"kimi", "--session", v} },
	"grok":       func(v string) []string { return []string{"grok", "--resume", v} },
	"copilot":    func(v string) []string { return []string{"copilot", "--resume=" + v} },
	"devin":      func(v string) []string { return []string{"devin", "--resume", v} },
	"droid":      func(v string) []string { return []string{"droid", "--resume", v} },
	"qwen":       func(v string) []string { return []string{"qwen", "--resume", v} },
	"hermes":     func(v string) []string { return []string{"hermes", "--resume", v} },
	"mastracode": func(v string) []string { return []string{"mastracode", "--thread", v} },
}

var (
	ErrNoResume      = errors.New("no resume reference")
	ErrUnknownAgent  = errors.New("agent has no known resume command")
	ErrInvalidResume = errors.New("resume command rejected")
)

// ResumeArgv picks the command that reopens a pane's agent session. A
// reported resume command wins over the built-in table, mirroring Herdr.
func ResumeArgv(r *AgentResume, s *AgentSession) ([]string, error) {
	var argv []string
	switch {
	case r != nil && len(r.Argv) > 0:
		argv = r.Argv
	case s != nil && s.Value != "":
		build, ok := nativeResume[s.Agent]
		if !ok {
			return nil, ErrUnknownAgent
		}
		argv = build(s.Value)
	default:
		return nil, ErrNoResume
	}
	if err := validateArgv(argv); err != nil {
		return nil, err
	}
	return argv, nil
}

// validateArgv applies Herdr's own resume rules: the first word is a plain
// command name and no argument carries control characters.
func validateArgv(argv []string) error {
	if len(argv) == 0 || len(argv) > 64 {
		return ErrInvalidResume
	}
	if argv[0] == "" || strings.ContainsAny(argv[0], "/\\ ") {
		return ErrInvalidResume
	}
	total := 0
	for _, a := range argv {
		total += len(a)
		for _, r := range a {
			if unicode.IsControl(r) {
				return ErrInvalidResume
			}
		}
	}
	if total > 8<<10 {
		return ErrInvalidResume
	}
	return nil
}

// ShellQuote joins argv into a POSIX shell command line that the pane's
// shell parses back into exactly argv.
func ShellQuote(argv []string) string {
	parts := make([]string, len(argv))
	for i, a := range argv {
		parts[i] = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
	}
	return strings.Join(parts, " ")
}
