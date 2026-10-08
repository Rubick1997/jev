package run

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/borislemeec/jev/internal/state"
	"github.com/borislemeec/jev/internal/typesafe"
)

// Route classifies each prompt into a model tier and tells the main session
// which tier should do the work. Claude Code cannot switch the main model from
// a hook, so the lever is delegation: a simple task goes to a subagent on a
// cheaper model while the main session keeps planning and review.
//
// Off by default; `jev route on` enables it for the current session.
//
//	jev route prompt      UserPromptSubmit hook (payload on stdin)
//	jev route on|off|status [--global]
//	jev route test "<prompt>"
func Route(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: jev route prompt|on|off|status|test")
	}
	switch args[0] {
	case "on", "off", "status":
		return Switch("route", args)
	case "prompt":
		return routeHook()
	case "test":
		if len(args) < 2 {
			return fmt.Errorf(`usage: jev route test "<prompt>"`)
		}
		r, err := classifyTier(strings.Join(args[1:], " "))
		if err != nil {
			return err
		}
		fmt.Printf("%s  %.2f  %v\n", r.tier, r.conf, r.probs)
		return nil
	}
	return fmt.Errorf("usage: jev route prompt|on|off|status|test")
}

var tiers = map[string]string{
	"haiku": "Mechanical and fully specified: a lookup, a rename, formatting, a one-line or single-file edit " +
		"whose exact change is stated, running a command and reporting its output, a factual question about the code.",
	"sonnet": "Ordinary engineering with a clear goal: implement a well-defined feature or test across a few files, " +
		"fix a bug with a known cause, write docs, refactor along a stated pattern.",
	"opus": "Needs judgment: ambiguous or open-ended requests, design decisions within one area, debugging an unknown " +
		"cause, multi-step plans across many files, reviewing or critiquing work.",
	"fable": "A wrong call is expensive: strategy, system architecture, auth or security, production data migrations, " +
		"or changes to shared infrastructure or config that other projects depend on.",
}

type tierResult struct {
	tier  string
	conf  float64
	probs map[string]float64
}

func classifyTier(prompt string) (tierResult, error) {
	client, err := typesafe.New()
	if err != nil {
		return tierResult{}, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	resp, err := client.Ask(ctx, map[string]any{"request": prompt}, map[string]typesafe.Question{
		"tier": typesafe.Choice("Which model tier is the cheapest one that would do the task in `request` well?", tiers),
	})
	if err != nil {
		return tierResult{}, err
	}
	a := resp.Answers["tier"]
	return tierResult{tier: a.Choice, conf: a.Confidence, probs: a.Probabilities}, nil
}

func routeHook() error {
	var in struct {
		Prompt    string `json:"prompt"`
		SessionID string `json:"session_id"`
	}
	empty := func() error { return nil } // no output: the prompt goes through untouched
	if json.NewDecoder(os.Stdin).Decode(&in) != nil {
		return empty()
	}
	if !state.Enabled(in.SessionID, func(s state.Switches) *bool { return s.Route }, false) {
		return empty()
	}
	p := strings.TrimSpace(in.Prompt)
	if skipPrompt(p) {
		return empty()
	}
	if len(p) > 4000 {
		p = p[:4000]
	}
	cwd, _ := os.Getwd()
	r, err := classifyTier(p)
	if err != nil {
		state.Record(state.Decision{Kind: "route", Session: in.SessionID, CWD: cwd, Outcome: "error", Reason: err.Error()})
		return empty()
	}
	minConf := 0.75
	if v, err := strconv.ParseFloat(os.Getenv("JEV_ROUTE_MIN_CONF"), 64); err == nil && v > 0 {
		minConf = v
	}
	outcome := "routed"
	if r.conf < minConf {
		outcome = "uncertain"
	}
	state.Record(state.Decision{Kind: "route", Session: in.SessionID, CWD: cwd, Outcome: outcome, Conf: r.conf,
		Extra: map[string]any{"tier": r.tier, "probs": r.probs, "prompt": truncateStr(p, 200)}})
	if outcome != "routed" {
		return empty()
	}

	return json.NewEncoder(os.Stdout).Encode(map[string]any{
		"hookSpecificOutput": map[string]any{"hookEventName": "UserPromptSubmit", "additionalContext": routeNote(r.tier, r.conf)},
	})
}

func routeNote(tier string, conf float64) string {
	switch tier {
	case "opus":
		return fmt.Sprintf("[jev route] Classified as needing judgment (opus, %.2f): handle this in the main session.", conf)
	case "fable":
		return fmt.Sprintf("[jev route] Classified as fable-tier (%.2f): a wrong call here is expensive. Do the work "+
			"through a subagent (Agent tool, model: \"fable\") with a self-contained prompt, then check its result "+
			"before replying. Keep final verification in this session.", conf)
	default:
		return fmt.Sprintf("[jev route] Classified as %s-tier (%.2f). Unless you see a reason it needs more judgment, "+
			"do the work through a subagent (Agent tool, model: %q) with a self-contained prompt, then check its "+
			"result before replying. Keep any planning, decisions and final verification in this session.",
			tier, conf, tier)
	}
}

// skipPrompt reports prompts whose meaning lives in the conversation rather
// than the text: slash commands, shell (!) and memory (#) input, system turns
// such as task notifications (<), and short follow-up replies.
func skipPrompt(p string) bool {
	if p == "" || strings.ContainsRune("/!#<", rune(p[0])) || len(p) < 20 {
		return true
	}
	return isFollowUp(p)
}

var (
	followUpStart = setOf("yes", "yeah", "yep", "no", "nope", "ok", "okay", "sure", "do", "go",
		"great", "thanks", "perfect", "nice", "good", "fine", "agreed", "also",
		"and", "but", "now", "then", "instead", "actually", "same", "again", "undo")
	followUpWords = setOf("that", "it", "this", "those", "these", "them", "shorter", "longer",
		"above", "previous", "last", "earlier", "same", "again", "instead")
	wordRe = regexp.MustCompile(`[a-z']+`)
)

func isFollowUp(p string) bool {
	words := wordRe.FindAllString(strings.ToLower(p), -1)
	if len(words) == 0 || len(words) > 12 {
		return false
	}
	if followUpStart[words[0]] {
		return true
	}
	// Reference words only count in very short replies: "that" is also a relative pronoun.
	if len(words) <= 6 {
		for _, w := range words {
			if followUpWords[w] {
				return true
			}
		}
	}
	return false
}

func setOf(words ...string) map[string]bool {
	m := make(map[string]bool, len(words))
	for _, w := range words {
		m[w] = true
	}
	return m
}

func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
