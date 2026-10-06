// Package state holds per-session switches and the decision log shared by the
// hooks: whether the Read hook may narrow, whether prompts are routed, and a
// JSONL record of every narrowing and routing decision for later tuning.
package state

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Dir is jev's data directory: $JEV_DATA, else ~/.jev.
func Dir() string {
	if d := strings.TrimSpace(os.Getenv("JEV_DATA")); d != "" {
		return d
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".jev"
	}
	return filepath.Join(home, ".jev")
}

// Switches are the toggles a session can flip. A nil field means "inherit":
// session overrides global, global overrides the built-in default.
type Switches struct {
	ReadHook *bool `json:"read_hook,omitempty"`
	Route    *bool `json:"route,omitempty"`
}

func switchesPath(session string) string {
	if session == "" {
		return filepath.Join(Dir(), "switches.json")
	}
	return filepath.Join(Dir(), "sessions", session+".json")
}

func load(session string) Switches {
	var s Switches
	if b, err := os.ReadFile(switchesPath(session)); err == nil {
		_ = json.Unmarshal(b, &s)
	}
	return s
}

// Save writes one scope's switches. An empty session is the global scope.
func Save(session string, s Switches) error {
	p := switchesPath(session)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(s, "", "  ")
	return os.WriteFile(p, b, 0o644)
}

// Load returns one scope's switches as stored, without inheritance.
func Load(session string) Switches { return load(session) }

// Enabled resolves a switch for a session: session value, then global value,
// then def.
func Enabled(session string, pick func(Switches) *bool, def bool) bool {
	if session != "" {
		if v := pick(load(session)); v != nil {
			return *v
		}
	}
	if v := pick(load("")); v != nil {
		return *v
	}
	return def
}

// CurrentSession is the session id of the Claude Code process running this
// command, when it runs from Claude's Bash tool.
func CurrentSession() string { return strings.TrimSpace(os.Getenv("CLAUDE_CODE_SESSION_ID")) }

// Decision is one line of ~/.jev/decisions.jsonl.
type Decision struct {
	TS      time.Time      `json:"ts"`
	Kind    string         `json:"kind"` // read, reread, route, compact
	Session string         `json:"session,omitempty"`
	CWD     string         `json:"cwd,omitempty"`
	File    string         `json:"file,omitempty"`
	Outcome string         `json:"outcome"` // narrowed, pass, routed, ...
	Reason  string         `json:"reason,omitempty"`
	Conf    float64        `json:"conf,omitempty"`
	Extra   map[string]any `json:"extra,omitempty"`
}

func DecisionsPath() string { return filepath.Join(Dir(), "decisions.jsonl") }

// Record appends a decision. Logging never fails the caller: a hook that broke
// a Read because its log was unwritable would be worse than no log.
func Record(d Decision) {
	if os.Getenv("JEV_NO_STATS") != "" {
		return
	}
	if d.TS.IsZero() {
		d.TS = time.Now().UTC()
	}
	if err := os.MkdirAll(Dir(), 0o755); err != nil {
		return
	}
	f, err := os.OpenFile(DecisionsPath(), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	b, _ := json.Marshal(d)
	f.Write(append(b, '\n'))
}

// ReadDecisions loads the whole log. It is small: one line per hook decision.
func ReadDecisions() []Decision {
	b, err := os.ReadFile(DecisionsPath())
	if err != nil {
		return nil
	}
	var out []Decision
	for _, line := range strings.Split(string(b), "\n") {
		var d Decision
		if json.Unmarshal([]byte(line), &d) == nil && d.Kind != "" {
			out = append(out, d)
		}
	}
	return out
}

// Narrowing remembers the last narrowed window per session and file, so the
// next Read of the same file can be recognised as a re-read: the signal that a
// window missed what the agent needed.
type Narrowing struct {
	Offset int       `json:"offset"`
	Limit  int       `json:"limit"`
	Conf   float64   `json:"conf"`
	TS     time.Time `json:"ts"`
}

func narrowPath(session string) string {
	return filepath.Join(Dir(), "sessions", session+".narrowed.json")
}

func Narrowings(session string) map[string]Narrowing {
	m := map[string]Narrowing{}
	if session == "" {
		return m
	}
	if b, err := os.ReadFile(narrowPath(session)); err == nil {
		_ = json.Unmarshal(b, &m)
	}
	return m
}

func SaveNarrowings(session string, m map[string]Narrowing) {
	if session == "" {
		return
	}
	p := narrowPath(session)
	if os.MkdirAll(filepath.Dir(p), 0o755) != nil {
		return
	}
	b, _ := json.Marshal(m)
	_ = os.WriteFile(p, b, 0o644)
}
