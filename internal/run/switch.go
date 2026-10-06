package run

import (
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/borislemeec/jev/internal/state"
)

// Switch flips one toggle (read_hook or route) for the current Claude Code
// session, or for every session with --global.
//
//	jev hook off            this session only
//	jev hook on --global    default for all sessions
//	jev route status
func Switch(name string, args []string) error {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	global := fs.Bool("global", false, "apply to every session, not just this one")
	session := fs.String("session", state.CurrentSession(), "session id (defaults to $CLAUDE_CODE_SESSION_ID)")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	pick := func(s *state.Switches) **bool {
		if name == "route" {
			return &s.Route
		}
		return &s.ReadHook
	}
	def := name == "read_hook" // the Read hook defaults on, routing off

	if args[0] == "status" {
		get := func(s state.Switches) *bool { return *pick(&s) }
		show := func(v *bool) string {
			if v == nil {
				return "inherit"
			}
			if *v {
				return "on"
			}
			return "off"
		}
		fmt.Printf("%s: %s  (session %s: %s, global: %s, default: %v)\n", name,
			map[bool]string{true: "ON", false: "OFF"}[state.Enabled(*session, get, def)],
			short(*session), show(get(state.Load(*session))), show(get(state.Load(""))), map[bool]string{true: "on", false: "off"}[def])
		return nil
	}

	scope := *session
	if *global {
		scope = ""
	} else if scope == "" {
		return fmt.Errorf("no session id: run inside Claude Code, pass --session, or use --global")
	}
	s := state.Load(scope)
	v := args[0] == "on"
	*pick(&s) = &v
	if err := state.Save(scope, s); err != nil {
		return err
	}
	where := "session " + short(scope)
	if scope == "" {
		where = "all sessions (global default)"
	}
	fmt.Printf("%s %s for %s\n", name, args[0], where)
	return nil
}

func short(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	if id == "" {
		return "-"
	}
	return id
}

// HookStats summarises the Read hook's decision log so its floors can be tuned:
// how often it narrowed, at what confidence, and how often a narrowing was
// followed by a re-read outside the window (the miss signal).
func HookStats(args []string) error {
	fs := flag.NewFlagSet("stats", flag.ContinueOnError)
	cwd := fs.String("cwd", "", "only decisions made in this project directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	var narrowed, passed, total int
	rereads := map[string]int{}
	buckets := map[string][2]int{} // confidence bucket -> {narrowed, missed}
	reasons := map[string]int{}
	missByFile := map[string]bool{}
	bucket := func(c float64) string {
		switch {
		case c >= 0.9:
			return "0.90+"
		case c >= 0.8:
			return "0.80-0.89"
		case c >= 0.7:
			return "0.70-0.79"
		case c >= 0.6:
			return "0.60-0.69"
		default:
			return "<0.60"
		}
	}
	for _, d := range state.ReadDecisions() {
		if *cwd != "" && d.CWD != *cwd {
			continue
		}
		switch d.Kind {
		case "read":
			total++
			if d.Outcome == "narrowed" {
				narrowed++
				b := buckets[bucket(d.Conf)]
				b[0]++
				buckets[bucket(d.Conf)] = b
			} else {
				passed++
				r := d.Reason
				if i := strings.IndexAny(r, "0123456789("); i > 0 {
					r = strings.TrimSpace(r[:i])
				}
				reasons[r]++
			}
		case "reread":
			rereads[d.Outcome]++
			if d.Outcome != "inside" {
				b := buckets[bucket(d.Conf)]
				b[1]++
				buckets[bucket(d.Conf)] = b
				missByFile[d.File] = true
			}
		}
	}
	if total == 0 {
		fmt.Printf("no Read-hook decisions logged yet (%s)\n", state.DecisionsPath())
		return nil
	}
	fmt.Printf("Read hook — %d large reads considered: %d narrowed, %d passed through\n", total, narrowed, passed)
	misses := rereads["outside"] + rereads["full"]
	if narrowed > 0 {
		fmt.Printf("re-reads after narrowing: %d outside window, %d full file, %d paging inside  → miss rate %.0f%%\n",
			rereads["outside"], rereads["full"], rereads["inside"], 100*float64(misses)/float64(narrowed))
	}
	fmt.Println("\nby confidence    narrowed  missed")
	keys := make([]string, 0, len(buckets))
	for k := range buckets {
		keys = append(keys, k)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(keys)))
	for _, k := range keys {
		b := buckets[k]
		fmt.Printf("  %-12s %8d %7d\n", k, b[0], b[1])
	}
	if len(reasons) > 0 {
		fmt.Println("\npass-through reasons")
		for r, n := range reasons {
			fmt.Printf("  %4d  %s\n", n, r)
		}
	}
	if len(missByFile) > 0 {
		fmt.Println("\nfiles with a miss (consider JEV_HOOK_MIN_CONF higher, or excluding them):")
		for f := range missByFile {
			fmt.Println("  " + f)
		}
	}
	fmt.Fprintf(os.Stderr, "\nlog: %s\n", state.DecisionsPath())
	return nil
}
