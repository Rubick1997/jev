package run

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/borislemeec/jev/internal/state"
	"github.com/borislemeec/jev/internal/typesafe"
)

// Compact turns a session transcript into a handoff file by keeping or dropping
// each piece of it, instead of summarising. Nothing is rewritten: what survives
// is verbatim, which is the point — a summary can misstate a decision, a kept
// paragraph cannot. User requests are always kept, edits are kept as one line
// each, and the most recent units are kept regardless of score.
//
//	jev compact [--goal "..."] [--min 0.5] [--transcript PATH]   write the handoff
//	jev compact inject                                         SessionStart hook
//
// Claude Code's own /compact cannot be replaced from a hook (PreCompact can only
// block), so the flow is: /jev:compact → /clear → the SessionStart hook loads
// the handoff into the fresh session.
func Compact(args []string) error {
	if len(args) > 0 && args[0] == "inject" {
		return compactInject()
	}
	fs := flag.NewFlagSet("compact", flag.ContinueOnError)
	goal := fs.String("goal", "", "what the next session continues (default: the latest user requests)")
	minScore := fs.Float64("min", 0.6, "keep units scoring at least this")
	transcript := fs.String("transcript", "", "transcript path (default: this session's)")
	keepLast := fs.Int("keep-last", 6, "always keep this many most recent units")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cwd, _ := os.Getwd()
	session := state.CurrentSession()
	if *transcript == "" {
		*transcript = findTranscript(cwd, session)
		if *transcript == "" {
			return fmt.Errorf("no transcript found for session %q; pass --transcript", session)
		}
	}
	units, requests, err := parseTranscript(*transcript)
	if err != nil {
		return err
	}
	if *goal == "" {
		n := len(requests)
		*goal = strings.Join(requests[max(0, n-3):], "\n---\n")
	}
	if len(*goal) > 3000 {
		*goal = (*goal)[len(*goal)-3000:]
	}

	client, err := typesafe.New()
	if err != nil {
		return err
	}
	start := time.Now()
	scores := make([]float64, len(units))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 16)
	q := typesafe.Noul("Would someone continuing the task in `goal` need the information in `unit` — a decision, " +
		"a finding, a constraint, a result, a file location, or an open question — that is not obvious from the code itself?")
	for i, u := range units {
		if u.always {
			scores[i] = 1
			continue
		}
		wg.Add(1)
		go func(i int, u unit) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			resp, err := client.Ask(context.Background(), map[string]any{"goal": *goal, "unit": u.text},
				map[string]typesafe.Question{"keep": q})
			if err != nil {
				scores[i] = 1 // on doubt keep: dropping is the irreversible mistake
				return
			}
			scores[i] = resp.Answers["keep"].Noul
		}(i, u)
	}
	wg.Wait()

	var b strings.Builder
	fmt.Fprintf(&b, "# Session handoff (jev compact)\n\nFrom session %s in `%s`, %s. Kept verbatim by relevance; nothing below is paraphrased.\n\n",
		short(session), cwd, time.Now().Format("2006-01-02 15:04"))
	b.WriteString("## User requests, in order\n\n")
	for i, r := range requests {
		fmt.Fprintf(&b, "%d. %s\n", i+1, indentCont(truncateStr(r, 1500)))
	}
	b.WriteString("\n## Kept context\n\n")
	kept, keptBytes, allBytes := 0, 0, 0
	touched := map[string]bool{}
	for i, u := range units {
		allBytes += len(u.text)
		for _, f := range u.files {
			touched[f] = true
		}
		if u.always && u.kind == "request" {
			continue // already listed above
		}
		if scores[i] >= *minScore || i >= len(units)-*keepLast {
			kept++
			keptBytes += len(u.text)
			fmt.Fprintf(&b, "- **%s** (%.2f): %s\n", u.kind, scores[i], indentCont(u.text))
		}
	}
	if len(touched) > 0 {
		b.WriteString("\n## Files touched\n\n")
		fs := make([]string, 0, len(touched))
		for f := range touched {
			fs = append(fs, f)
		}
		sort.Strings(fs)
		for _, f := range fs {
			fmt.Fprintf(&b, "- `%s`\n", f)
		}
	}
	fmt.Fprintf(&b, "\nFull transcript: `%s`\n", *transcript)

	out := handoffPath(cwd)
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(out, []byte(b.String()), 0o644); err != nil {
		return err
	}
	brief := briefHandoff(units, scores, requests, *keepLast, out)
	if err := os.WriteFile(briefPath(out), []byte(brief), 0o644); err != nil {
		return err
	}
	state.Record(state.Decision{Kind: "compact", Session: session, CWD: cwd, Outcome: "written",
		Extra: map[string]any{"units": len(units), "kept": kept, "bytes_in": allBytes, "bytes_kept": keptBytes, "out": out}})
	fmt.Printf("%d units scored in %.1fs, %d kept (%d KB → %d KB full, %d KB injected)\nhandoff: %s\nNext: run /clear; the new session loads this automatically.\n",
		len(units), time.Since(start).Seconds(), kept, allBytes/1024, b.Len()/1024, len(brief)/1024, out)
	return nil
}

type unit struct {
	kind   string // request, said, tool, result, notice
	text   string
	always bool
	files  []string
}

var reminder = regexp.MustCompile(`(?s)<system-reminder>.*?</system-reminder>`)

// parseTranscript flattens a Claude Code JSONL transcript into units. Thinking,
// sidechains, attachments other than queued prompts, and bookkeeping entries
// are dropped before scoring; they are never what a handoff needs.
func parseTranscript(path string) ([]unit, []string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()
	var units []unit
	var requests []string
	toolNames := map[string]string{}
	request := func(t string) {
		t = strings.TrimSpace(reminder.ReplaceAllString(t, ""))
		if t == "" || strings.HasPrefix(t, "<local-command") || strings.HasPrefix(t, "<command-") {
			return
		}
		if strings.HasPrefix(t, "<task-notification>") {
			units = append(units, unit{kind: "notice", text: truncateStr(t, 4000)})
			return
		}
		requests = append(requests, t)
		units = append(units, unit{kind: "request", text: t, always: true})
	}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 64<<20)
	for sc.Scan() {
		var o struct {
			Type        string `json:"type"`
			IsSidechain bool   `json:"isSidechain"`
			IsMeta      bool   `json:"isMeta"`
			Attachment  struct {
				Type   string `json:"type"`
				Prompt string `json:"prompt"`
			} `json:"attachment"`
			Message struct {
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(sc.Bytes(), &o) != nil || o.IsSidechain || o.IsMeta {
			continue
		}
		switch o.Type {
		case "attachment":
			if o.Attachment.Type == "queued_command" {
				request(o.Attachment.Prompt)
			}
		case "user", "assistant":
			var s string
			if json.Unmarshal(o.Message.Content, &s) == nil {
				if o.Type == "user" {
					request(s)
				}
				continue
			}
			var blocks []map[string]any
			if json.Unmarshal(o.Message.Content, &blocks) != nil {
				continue
			}
			for _, bl := range blocks {
				switch bl["type"] {
				case "text":
					t, _ := bl["text"].(string)
					if o.Type == "user" {
						request(t)
						continue
					}
					for _, para := range strings.Split(t, "\n\n") {
						if p := strings.TrimSpace(para); len(p) > 3 {
							units = append(units, unit{kind: "said", text: p})
						}
					}
				case "tool_use":
					name, _ := bl["name"].(string)
					id, _ := bl["id"].(string)
					toolNames[id] = name
					in, _ := bl["input"].(map[string]any)
					u := unit{kind: "tool", text: name + " " + describeInput(in)}
					if fp, ok := in["file_path"].(string); ok {
						u.files = []string{fp}
						if name == "Edit" || name == "Write" || name == "NotebookEdit" {
							u.always = true
							u.text = name + " " + fp
						}
					}
					units = append(units, u)
				case "tool_result":
					id, _ := bl["tool_use_id"].(string)
					if n := toolNames[id]; n == "Read" || n == "Edit" || n == "Write" {
						continue // the file is on disk; its old bytes are what compaction exists to drop
					}
					units = append(units, unit{kind: "result", text: truncateStr(strings.TrimSpace(resultText(bl["content"])), 2000)})
				}
			}
		}
	}
	return units, requests, sc.Err()
}

func describeInput(in map[string]any) string {
	for _, k := range []string{"command", "file_path", "pattern", "description", "prompt", "url", "query", "skill"} {
		if v, ok := in[k].(string); ok && v != "" {
			return truncateStr(strings.ReplaceAll(v, "\n", " "), 300)
		}
	}
	b, _ := json.Marshal(in)
	return truncateStr(string(b), 300)
}

func resultText(c any) string {
	switch v := c.(type) {
	case string:
		return v
	case []any:
		var b strings.Builder
		for _, x := range v {
			if m, ok := x.(map[string]any); ok {
				if t, ok := m["text"].(string); ok {
					b.WriteString(t)
					b.WriteString("\n")
				}
			}
		}
		return b.String()
	}
	return ""
}

// injectBudget keeps the brief under the 10,000-character hook context cap.
const injectBudget = 9000

func briefPath(full string) string { return strings.TrimSuffix(full, ".md") + ".brief.md" }

// briefHandoff is what the next session actually receives: every request
// (they are the intent), then the highest-scoring units that fit the budget,
// shown in their original order so the story still reads forward.
func briefHandoff(units []unit, scores []float64, requests []string, keepLast int, full string) string {
	var head strings.Builder
	head.WriteString("## User requests, in order\n\n")
	for i, r := range requests {
		fmt.Fprintf(&head, "%d. %s\n", i+1, indentCont(truncateStr(r, 600)))
	}
	head.WriteString("\n## Most relevant context (verbatim, highest-scoring first by budget)\n\n")
	foot := fmt.Sprintf("\nThe full handoff, with every kept unit and the files touched, is at `%s`. Read it for anything missing.\n", full)
	budget := injectBudget - head.Len() - len(foot)

	idx := make([]int, 0, len(units))
	for i, u := range units {
		if u.kind != "request" {
			idx = append(idx, i)
		}
	}
	// Recent units first, then by score: the end of a session is where work stopped.
	recent := map[int]bool{}
	for k := len(idx) - 1; k >= 0 && len(recent) < keepLast; k-- {
		recent[idx[k]] = true
	}
	sort.SliceStable(idx, func(a, b int) bool {
		ra, rb := recent[idx[a]], recent[idx[b]]
		if ra != rb {
			return ra
		}
		return scores[idx[a]] > scores[idx[b]]
	})
	chosen := map[int]bool{}
	for _, i := range idx {
		line := fmt.Sprintf("- **%s**: %s\n", units[i].kind, indentCont(truncateStr(units[i].text, 700)))
		if len(line) > budget {
			continue
		}
		budget -= len(line)
		chosen[i] = true
	}
	var body strings.Builder
	for i := range units {
		if chosen[i] {
			fmt.Fprintf(&body, "- **%s**: %s\n", units[i].kind, indentCont(truncateStr(units[i].text, 700)))
		}
	}
	return head.String() + body.String() + foot
}

func indentCont(s string) string { return strings.ReplaceAll(s, "\n", "\n  ") }

// findTranscript locates this session's JSONL under ~/.claude/projects.
func findTranscript(cwd, session string) string {
	home, _ := os.UserHomeDir()
	if session != "" {
		esc := regexp.MustCompile(`[^A-Za-z0-9-]`).ReplaceAllString(cwd, "-")
		p := filepath.Join(home, ".claude", "projects", esc, session+".jsonl")
		if _, err := os.Stat(p); err == nil {
			return p
		}
		if m, _ := filepath.Glob(filepath.Join(home, ".claude", "projects", "*", session+".jsonl")); len(m) > 0 {
			return m[0]
		}
	}
	return ""
}

func handoffPath(cwd string) string {
	esc := regexp.MustCompile(`[^A-Za-z0-9-]`).ReplaceAllString(cwd, "-")
	return filepath.Join(state.Dir(), "handoff", esc+".md")
}

// compactInject is the SessionStart hook: after /clear (or a fresh start
// within 15 minutes) it loads this project's unconsumed handoff, then marks it
// consumed so it is injected once.
func compactInject() error {
	var in struct {
		Source string `json:"source"`
		CWD    string `json:"cwd"`
	}
	if json.NewDecoder(os.Stdin).Decode(&in) != nil {
		return nil
	}
	if in.CWD == "" {
		in.CWD, _ = os.Getwd()
	}
	full := handoffPath(in.CWD)
	p := briefPath(full)
	st, err := os.Stat(p)
	if err != nil {
		return nil
	}
	age := time.Since(st.ModTime())
	if !(in.Source == "clear" && age < 2*time.Hour) && !(in.Source == "startup" && age < 15*time.Minute) {
		return nil
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return nil
	}
	// Consume once: a second /clear should start clean, not replay this.
	_ = os.Rename(p, strings.TrimSuffix(p, ".md")+".used.md")
	body := string(b)
	if len(body) > injectBudget+500 {
		body = body[:injectBudget] + "\n\n[truncated — see " + full + "]"
	}
	return json.NewEncoder(os.Stdout).Encode(map[string]any{
		"hookSpecificOutput": map[string]any{
			"hookEventName":     "SessionStart",
			"additionalContext": "Handoff from the previous session (jev compact). Continue from here:\n\n" + body,
		},
	})
}
