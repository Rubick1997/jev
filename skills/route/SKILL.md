---
name: route
description: Turn jev model routing on or off for this session. When on, every prompt is classified into haiku/sonnet/opus tiers by Jev, and simple tasks are delegated to a subagent on a cheaper model. Use for "/jev:route", "turn on jev routing", "route cheap tasks to haiku".
argument-hint: on | off | status | test "<prompt>" [--global]
---

Run `jev route $ARGUMENTS` (default `status`) and report the result in one line.

When routing is on, a UserPromptSubmit hook adds a line like `[jev route] Classified as haiku-tier (0.97)...` to each prompt (over 20 characters, not a slash command) whose classification reaches 0.75 confidence (`JEV_ROUTE_MIN_CONF`). When you see that line:
- **haiku / sonnet**: do the work through the Agent tool with `model` set to that tier and a self-contained prompt (files, exact change, how to verify). Review the subagent's result, then reply. Planning, decisions, and verification stay in the main session. Override the routing if the task clearly needs more judgment than the label suggests, and say so in one short clause.
- **opus**: work in the main session as usual.

`jev route test "<prompt>"` shows the classification without enabling anything. Routing decisions are logged in `~/.jev/decisions.jsonl` (kind `route`).
