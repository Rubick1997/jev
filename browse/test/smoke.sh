#!/usr/bin/env bash
# Smoke test for jev-browse against the local fixture (file://, no server).
# Needs a working `jev decide` (TypeSafe key in the keychain). Exit 0 = all pass.
set -uo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
root="$(cd "$here/../.." && pwd)"
J="$root/bin/jev-browse"
URL="file://$here/fixture.html"
tmp="$(mktemp -d)"; trap 'rm -rf "$tmp"; "$J" --close smoke-plan >/dev/null; "$J" --close smoke-step >/dev/null; "$J" --close smoke-amb >/dev/null' EXIT
pass=0; failn=0
check() { # name, json-file, js-expression over r
  if node -e "const r=JSON.parse(require('fs').readFileSync('$2','utf8')); if(!($3)) process.exit(1)"; then
    echo "PASS $1"; pass=$((pass+1))
  else
    echo "FAIL $1"; cat "$2"; echo; failn=$((failn+1))
  fi
}
conf() { node -e "const r=JSON.parse(require('fs').readFileSync('$1','utf8')); console.log('   ', r.status, (r.log||[]).map(e=>e.step+':'+e.action+' '+JSON.stringify(e.confidence)).join('  '), r.handback? '| handback: '+r.handback.reason : '')"; }

echo "== 1. plan mode: whole login flow in one call"
cat > "$tmp/plan.json" <<JSON
{"url": "$URL", "goal": "sign in to Acme", "mode": "plan", "minConfidence": 0.9,
 "steps": [
  {"do": "fill the email field", "value": "a@b.com"},
  {"do": "fill the password field", "value": "hunter2"},
  {"do": "choose Europe in the region dropdown", "value": "Europe"},
  {"do": "click the Sign in button"},
  {"expect": "the dashboard is shown and the user is signed in as a@b.com"}
 ]}
JSON
"$J" --session smoke-plan --reset --plan "$tmp/plan.json" --json > "$tmp/1.json"
conf "$tmp/1.json"
check "plan mode finishes all 5 steps" "$tmp/1.json" "r.status==='done' && r.stepsRun===5"
check "plan mode lands on dashboard with region eu" "$tmp/1.json" "/dashboard\\.html\\?email=a%40b\\.com&region=eu/.test(r.url)"

echo "== 2. step mode: one step per call, browser kept alive between calls"
"$J" --session smoke-step --reset --url "$URL" --do "open the Pricing page" --json > "$tmp/2a.json"; conf "$tmp/2a.json"
"$J" --session smoke-step --do "set the number of seats" --value 3 --json > "$tmp/2b.json"; conf "$tmp/2b.json"
echo '{"mode":"step","steps":[{"do":"click Get quote"},{"expect":"never run in step mode"}]}' \
  | "$J" --session smoke-step --plan - --json > "$tmp/2c.json"; conf "$tmp/2c.json"
"$J" --session smoke-step --expect "a quote of 36 dollars per month is shown" --json > "$tmp/2d.json"; conf "$tmp/2d.json"
check "step mode: link to second page" "$tmp/2a.json" "r.status==='done' && /page2\\.html$/.test(r.url)"
check "step mode: fill seats" "$tmp/2b.json" "r.status==='done' && r.log[0].action==='fill'"
check "step mode: runs exactly one step of a 2-step plan" "$tmp/2c.json" "r.status==='done' && r.stepsRun===1 && r.remaining===1"
check "step mode: typed value survived across calls" "$tmp/2d.json" "r.status==='done' && r.log[0].outcome==='pass'"

echo "== 3. ambiguous step: 'click the button' with two buttons must hand back, not click"
cat > "$tmp/amb.json" <<JSON
{"url": "$URL", "mode": "plan", "steps": [{"do": "click the button"}, {"expect": "anything"}]}
JSON
"$J" --session smoke-amb --reset --plan "$tmp/amb.json" --json > "$tmp/3.json"; conf "$tmp/3.json"
node -e "const r=JSON.parse(require('fs').readFileSync('$tmp/3.json','utf8')); console.log('    candidates:', JSON.stringify(r.handback&&r.handback.candidates))"
check "ambiguous step hands back" "$tmp/3.json" "r.status==='handback' && r.stepsRun===1 && r.handback.step===1"
check "ambiguous step executed nothing" "$tmp/3.json" "r.log[0].outcome==='handback' && /fixture\\.html$/.test(r.url)"
"$J" --session smoke-amb --expect "the text 'Create account was clicked' is shown on the page" --min-confidence 0.5 --json > "$tmp/3b.json"; conf "$tmp/3b.json"
check "no wrong button was clicked" "$tmp/3b.json" "r.status==='handback' && r.handback.reason.startsWith('expectation failed')"
"$J" --session smoke-amb --do "click Create account" --json > "$tmp/3c.json"; conf "$tmp/3c.json"
check "rewritten specific step then succeeds" "$tmp/3c.json" "r.status==='done' && /Create account/.test(r.log[0].element)"

echo "== 4. no live browser (JEV_BROWSE_LIVE=0): storageState + URL fallback"
JEV_BROWSE_LIVE=0 "$J" --session smoke-nolive --reset --url "$URL" --do "click the Pricing link" --json > "$tmp/4a.json"
JEV_BROWSE_LIVE=0 "$J" --session smoke-nolive --expect "this is the pricing page" --json > "$tmp/4b.json"; conf "$tmp/4b.json"
check "fallback session resumes at saved URL" "$tmp/4b.json" "r.status==='done' && /page2\\.html$/.test(r.url)"

echo "== $pass passed, $failn failed"
[ "$failn" -eq 0 ]
