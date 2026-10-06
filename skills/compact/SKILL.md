---
name: compact
description: Compact this session with jev - instead of summarizing, Jev keeps or drops each piece of the transcript verbatim, writes a handoff, and the next session loads it after /clear. Use for "/jev:compact", "jev compact", "hand off this session", or when context is large and the user wants a verbatim, not summarized, carry-over.
argument-hint: [goal for the next session]
---

1. Run `jev compact` with `--goal "$ARGUMENTS"` if arguments were given; otherwise run it with no flags (the goal becomes the latest user requests). Add `--min 0.7` if the user wants it tighter.
2. Report the line with the unit counts and sizes, plus the handoff path.
3. Tell the user to run `/clear` (or restart Claude Code within 15 minutes). The SessionStart hook injects the brief handoff (about 9 KB) once; the full handoff stays at the printed path.

Before running it, make sure anything important that only exists in this conversation is written down (for example, in the project's progress ledger), because dropped units are gone from the next session. All user requests, every Edit and Write, and the last 6 units are always kept.
