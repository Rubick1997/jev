---
name: browse
description: >
  Drive or automate a website or web app in a real browser without pulling pages,
  DOM dumps or screenshots into context. Use when asked to click through a flow,
  fill in or submit a form, log in and check a page, reproduce a UI bug, or check
  a UI flow end to end (local dev server, staging, file:// pages or public sites).
  You write a short plan in plain words; the cheap Jev model picks the element and
  action for each step and hands control back when it is not confident.
---

# browse: you plan, Jev clicks

`jev-browse` runs a browser. You never see the page unless something goes wrong:
for each step Jev (a cheap classifier that returns only probabilities)
looks at the visible interactive elements and picks which one the step means and
which action it is. A step runs only when Jev is confident; otherwise the runner
stops and hands the step back to you with the top candidates and a short element
list, so you can rewrite it.

A 5-step login flow costs about 5 Jev calls and adds ~10 lines to this context.

## Two modes

- **plan** (default): run every step in one call, stop at the first hand-back.
  Use it for short, predictable flows: a login, a form, "open X and check Y".
- **step**: run exactly one step per call. Use it for long or unfamiliar flows,
  when the next step depends on what the last one showed, or after a plan-mode
  run handed back twice. You decide each next step from the result.

Plan-once works for simple tasks and breaks on complex ones; when in doubt,
start in plan mode and fall back to step mode at the first hand-back.

## Plan format

```json
{
  "url": "http://localhost:3000/login",
  "goal": "sign in and open billing",
  "mode": "plan",
  "minConfidence": 0.9,
  "steps": [
    {"do": "fill the email field", "value": "a@b.com"},
    {"do": "fill the password field", "value": "hunter2"},
    {"do": "click the Sign in button"},
    {"expect": "the dashboard is shown"},
    {"do": "open the Billing link"},
    {"do": "press Enter", "key": "Enter"}
  ]
}
```

- `do` names one element by its visible label or role: "click the Sign in
  button", "fill the Search box", "choose Europe in the Region dropdown".
  Vague steps ("click the button" on a page with two buttons) hand back by
  design; that is the safety mechanism, not a bug.
- `value` is required for fill and select. **Values come only from the plan;**
  Jev never invents text. For select, `value` is the option label (or value).
- `key` for press (else it is read from "press Enter"); `url` for navigate.
- `expect` is a yes/no check over the page title and visible text. It passes
  at p >= minConfidence, fails at p <= 1 - minConfidence, else hands back.
- Optional per step: `"action": "click|fill|select|press|navigate"` skips the
  action question; `"selector": "css or playwright selector"` skips the element
  question (escape hatch after a hand-back you cannot fix by rewording);
  `"minConfidence"` overrides the plan default.
- Plan-level: `headless` (default true), `maxSteps` (30), `storageState`
  (path to a Playwright storage-state file to start from and write back to),
  `resume` (see Sessions).

## Running

```bash
jev-browse --plan plan.json --session app            # plan mode
jev-browse --plan - --session app <<'EOF'            # plan on stdin
{"url": "...", "steps": [...]}
EOF
# step mode without writing a file:
jev-browse --session app --url http://localhost:3000 --do "click Sign in"
jev-browse --session app --do "fill the email field" --value a@b.com
jev-browse --session app --expect "an error message about the password is shown"
jev-browse --close app                               # shut the session's browser
```

Add `--json` for a machine-readable result: `{status: "done"|"handback"|"error",
stepsRun, log: [{step, element, action, confidence}], handback?, screenshot, url}`.
Exit code: 0 done, 3 hand-back, 1 error, 2 bad arguments. Other flags:
`--min-confidence X`, `--headed` (visible window), `--reset` (forget the session),
`--step N` (run only step N of the plan), `--mode plan|step`.

## Sessions

`--session NAME` (default `default`) keeps one browser alive between calls, so
typed text, open menus and logins persist from one step-mode call to the next.
It shuts down after 15 idle minutes (`JEV_BROWSE_IDLE_MIN`). Cookies, local
storage and the current URL are also saved in `~/.jev/browse/NAME/`, so a later
call still resumes there after the browser is gone.

Where a call starts: step mode, or a plan with no `url`, continues on the
session's current page. A plan with `url` (or `--url`) loads that URL first.
Set `"resume": true|false` in the plan to force either. To continue a plan
after a hand-back, send the remaining steps **without `url`**.

## Reading the result

```
ok  1 fill e3 textbox "Email" (element 1.00, single 0.99, action 1.00)
ok  2 click e6 button "Sign in" (element 1.00, single 1.00, action 1.00)
ok  3 expect "the dashboard is shown" -> pass (p 0.98)
done 3 step(s) | http://localhost:3000/dashboard
screenshot: ~/.jev/browse/app/step-3.png
```

`element` is Jev's confidence in the chosen element, `single` the probability
that the wording fits exactly one element, `action` the confidence in the
action. All three must reach minConfidence.

Trust `done` lines; do not open the screenshot to double-check a run that
passed its `expect` steps. Add an `expect` step instead when you need proof.

## Handling a hand-back

```
!!  1 click e6 button "Sign in" (element 0.67, single 0.02, action 1.00)
HANDBACK at step 1: step may fit several elements (p(exactly one)=0.02 < 0.9)
  step: {"do":"click the button"}
  top candidates: e6 0.71 button "Sign in"; none 0.29; ...
  elements (7 of 7):
    e6 button "Sign in"
    e7 button "Create account"
    ...
screenshot: ~/.jev/browse/app/step-1.png
```

Nothing was executed for the step that handed back. Then:

1. Read the reason and the element list. Usually the fix is to rewrite the
   step to quote the exact label: `{"do": "click the Create account button"}`.
2. If the element is missing from the list, it may be off-screen, inside an
   iframe, or not rendered yet: add a step that opens or scrolls to it, or
   `expect` the state that should reveal it.
3. "expectation failed": the page is not in the state you assumed. Look at the
   `page text` line before re-planning; open the screenshot (Read the png) only
   if text is not enough.
4. "action failed": the element was found but did not respond (disabled,
   covered by an overlay). Dismiss the overlay or wait with an `expect` step.
5. As a last resort pin the element with `"selector"` or the action with `"action"`.
6. After two hand-backs in plan mode, switch to step mode for the rest of the flow.

Then rerun with the corrected step(s), without `url`, so the session continues
from where it stopped.

## Do not

- Do not lower `minConfidence` below about 0.8 to get past a hand-back; reword
  the step. A low threshold is how a wrong button gets clicked.
- Do not put secrets you were not given into plans. Plan values are typed
  verbatim; they are never written to the decision log.
- Do not describe Jev's choices as reasoning; it returns probabilities only.

Every decision is logged to `~/.jev/browse/decisions.jsonl`
(ts, session, step, chosen, confidence, executed, outcome).
