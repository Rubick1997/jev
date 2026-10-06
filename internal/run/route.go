package run

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
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
	"opus": "Needs judgment: ambiguous or open-ended requests, architecture or design decisions, debugging an unknown " +
		"cause, security- or data-sensitive changes, multi-step plans across many files, reviewing or critiquing work.",
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
	// Slash commands and one-word follow-ups ("yes", "continue") carry their
	// meaning in the conversation, not the text; classifying them is noise.
	if strings.HasPrefix(p, "/") || len(p) < 20 {
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

	var note string
	switch r.tier {
	case "opus":
		note = fmt.Sprintf("[jev route] Classified as needing judgment (opus, %.2f): handle this in the main session.", r.conf)
	default:
		note = fmt.Sprintf("[jev route] Classified as %s-tier (%.2f). Unless you see a reason it needs more judgment, "+
			"do the work through a subagent (Agent tool, model: %q) with a self-contained prompt, then check its "+
			"result before replying. Keep any planning, decisions and final verification in this session.",
			r.tier, r.conf, r.tier)
	}
	return json.NewEncoder(os.Stdout).Encode(map[string]any{
		"hookSpecificOutput": map[string]any{"hookEventName": "UserPromptSubmit", "additionalContext": note},
	})
}

func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
