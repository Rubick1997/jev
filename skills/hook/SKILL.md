---
name: hook
description: Turn jev's Read-narrowing hook on or off for this session, show its status, or show its decision stats (narrowings, confidence, misses). Use when the user says "/jev:hook", "turn off jev narrowing", "jev hook stats", or a narrowed Read hid something needed.
argument-hint: on | off | status | stats [--global]
---

Run exactly one command based on `$ARGUMENTS` (default `status`), then report its output in one or two lines:

- `on` / `off` / `status`: `jev hook $ARGUMENTS`. This applies to this session only unless `--global` is given (`--global` sets the default for all sessions).
- `stats`: `jev hook stats --cwd "$PWD"`, or `jev hook stats` for all projects when the user asks for everything. Summarize: how many reads were narrowed, the miss rate (re-reads outside the window), and the confidence buckets that missed. If misses cluster in a bucket, suggest raising `JEV_HOOK_MIN_CONF` above it, but don't change it yourself.

When the hook is off, Reads pass through untouched. The decision log keeps recording re-reads, so you can compare.
