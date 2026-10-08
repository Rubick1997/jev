package run

import (
	"strings"
	"testing"
)

func TestSkipPrompt(t *testing.T) {
	cases := []struct {
		prompt string
		skip   bool
	}{
		{"", true},
		{"/jev:route on please route this", true},
		{"!git status --short --branch", true},
		{"# always use pnpm in this repo", true},
		{"<task-notification>agent finished</task-notification>", true},
		{"rename it", true},
		{"yes, go ahead and do that for all of them", true},
		{"make the second one shorter", true},
		{"actually use the other endpoint for this", true},
		{"add a retry with backoff to the upload client", false},
		{"why does the login form submit twice when you press enter", false},
		{"move that helper into the shared utils package and update every import that uses it", false},
	}
	for _, c := range cases {
		if got := skipPrompt(c.prompt); got != c.skip {
			t.Errorf("skipPrompt(%q) = %v, want %v", c.prompt, got, c.skip)
		}
	}
}

func TestRouteNote(t *testing.T) {
	cases := []struct {
		tier, want, notWant string
	}{
		{"haiku", `model: "haiku"`, ""},
		{"sonnet", `model: "sonnet"`, ""},
		{"fable", `model: "fable"`, ""},
		{"opus", "main session", "Agent tool"},
	}
	for _, c := range cases {
		note := routeNote(c.tier, 0.9)
		if !strings.Contains(note, c.want) {
			t.Errorf("routeNote(%q) = %q, want it to contain %q", c.tier, note, c.want)
		}
		if c.notWant != "" && strings.Contains(note, c.notWant) {
			t.Errorf("routeNote(%q) = %q, should not contain %q", c.tier, note, c.notWant)
		}
	}
}
