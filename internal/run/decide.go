package run

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/borislemeec/jev/internal/typesafe"
)

// Decide is the raw primitive for scripts: one System One request read as JSON
// on stdin ({"state": ..., "questions": {...}}), the decoded response printed as
// JSON on stdout. It exists so tools in other languages reuse this binary's key
// resolution, backend choice, rate limiting and retries instead of copying them.
func Decide(args []string) error {
	fs := flag.NewFlagSet("decide", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, decideUsage) }
	if err := fs.Parse(args); err != nil {
		return err
	}
	raw, err := io.ReadAll(os.Stdin)
	if err != nil {
		return err
	}
	var req struct {
		State     any                          `json:"state"`
		Questions map[string]typesafe.Question `json:"questions"`
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		return fmt.Errorf("decide: stdin is not a {state, questions} object: %w", err)
	}
	client, err := typesafe.New()
	if err != nil {
		return err
	}
	resp, err := client.Ask(context.Background(), req.State, req.Questions)
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(resp)
}

const decideUsage = `usage: echo '{"state":{...},"questions":{"id":{"type":"noul","instructions":"..."}}}' | jev decide

Sends one request and prints the decoded response as JSON:
  {"model":"...","answers":{"id":{"type":"noul","noul":0.97}},"usage":{...}}

Question types:
  noul    {"type":"noul","instructions":"..."}                         probability of yes
  choice  {"type":"choice","instructions":"...","criteria":{"key":"description",...}}
  score   {"type":"score","instructions":"...","criteria":["level 1","level 2",...]}
`
