package lazy

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// Dormant is a restored pane whose agent session has not been resumed yet.
type Dormant struct {
	PaneID string   `json:"pane_id"`
	Cwd    string   `json:"cwd"`
	Agent  string   `json:"agent"`
	Argv   []string `json:"argv"`
}

type sessionFile struct {
	Workspaces []struct {
		ID                string            `json:"id"`
		PublicPaneNumbers map[string]uint64 `json:"public_pane_numbers"`
		Tabs              []struct {
			Panes map[string]struct {
				Cwd          string        `json:"cwd"`
				AgentSession *AgentSession `json:"agent_session"`
				AgentResume  *AgentResume  `json:"agent_resume"`
			} `json:"panes"`
		} `json:"tabs"`
	} `json:"workspaces"`
}

// Skipped describes a pane that had an agent reference we cannot resume.
type Skipped struct {
	PaneID string
	Reason string
}

// ParseSession reads Herdr's session.json and returns every pane that holds a
// resumable agent reference.
func ParseSession(r io.Reader) ([]Dormant, []Skipped, error) {
	var sf sessionFile
	if err := json.NewDecoder(r).Decode(&sf); err != nil {
		return nil, nil, fmt.Errorf("decode session.json: %w", err)
	}
	var out []Dormant
	var skipped []Skipped
	for _, ws := range sf.Workspaces {
		for _, tab := range ws.Tabs {
			for key, p := range tab.Panes {
				if p.AgentResume == nil && p.AgentSession == nil {
					continue
				}
				public, ok := ws.PublicPaneNumbers[key]
				if !ok {
					skipped = append(skipped, Skipped{ws.ID + "/" + key, "no public pane number"})
					continue
				}
				id := PaneID(ws.ID, public)
				argv, err := ResumeArgv(p.AgentResume, p.AgentSession)
				if err != nil {
					skipped = append(skipped, Skipped{id, err.Error()})
					continue
				}
				agent := ""
				if p.AgentResume != nil {
					agent = p.AgentResume.Agent
				} else {
					agent = p.AgentSession.Agent
				}
				out = append(out, Dormant{PaneID: id, Cwd: p.Cwd, Agent: agent, Argv: argv})
			}
		}
	}
	return out, skipped, nil
}

// ResumeOnRestoreDisabled reports whether config.toml sets
// [session] resume_agents_on_restore = false. Anything else, including a
// missing file, means Herdr resumes every agent itself.
func ResumeOnRestoreDisabled(r io.Reader) bool {
	section := ""
	disabled := false
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = strings.TrimSpace(line[:i])
		}
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "[") {
			section = strings.TrimSpace(strings.Trim(line, "[]"))
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key := strings.TrimSpace(k)
		if section != "" {
			key = section + "." + key
		}
		if key == "session.resume_agents_on_restore" {
			b, err := strconv.ParseBool(strings.TrimSpace(v))
			disabled = err == nil && !b
		}
	}
	return disabled
}
