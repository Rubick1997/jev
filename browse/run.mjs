#!/usr/bin/env node
// jev-browse: Claude writes the plan, Jev picks the element and action for each
// step, and the runner hands control back whenever Jev is not confident.
//
//   jev-browse --plan plan.json [--session NAME] [--json] [--mode plan|step]
//   jev-browse --session NAME --do "click Sign in" [--value V] [--json]
//   jev-browse --session NAME --expect "the dashboard is shown"
//
// Values typed into the page come only from the plan, never from Jev.

import { chromium } from 'playwright';
import { spawn } from 'node:child_process';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const HERE = path.dirname(fileURLToPath(import.meta.url));
const HOME_DIR = path.join(os.homedir(), '.jev', 'browse');
const DECISIONS = path.join(HOME_DIR, 'decisions.jsonl');
const MAX_CANDIDATES = 150;
const HANDBACK_ELEMENTS = 40;
const PAGE_TEXT_CHARS = 6000;
const ACTION_TIMEOUT = 5000;

const ACTIONS = {
  click: 'click, tap, open or follow a button, link, checkbox, radio, tab or other element',
  fill: 'type or enter text into a text field, search box or text area',
  select: 'choose an option in a dropdown / select list',
  press: 'press a keyboard key such as Enter, Tab or Escape',
  navigate: 'load a web address (URL) given in the step, as if typed into the address bar',
  none: 'none of these / unclear',
};

// ---------------------------------------------------------------- arguments

function parseArgs(argv) {
  const a = { json: false };
  for (let i = 0; i < argv.length; i++) {
    const k = argv[i];
    const next = () => {
      if (i + 1 >= argv.length) fail(`missing value for ${k}`);
      return argv[++i];
    };
    switch (k) {
      case '--plan': a.plan = next(); break;
      case '--session': a.session = next(); break;
      case '--json': a.json = true; break;
      case '--mode': a.mode = next(); break;
      case '--step': a.step = Number(next()); break;
      case '--do': a.do = next(); break;
      case '--expect': a.expect = next(); break;
      case '--value': a.value = next(); break;
      case '--key': a.key = next(); break;
      case '--selector': a.selector = next(); break;
      case '--action': a.action = next(); break;
      case '--url': a.url = next(); break;
      case '--min-confidence': a.minConfidence = Number(next()); break;
      case '--headed': a.headed = true; break;
      case '--reset': a.reset = true; break;
      case '-h': case '--help': usage(0); break;
      default: fail(`unknown argument: ${k}`);
    }
  }
  return a;
}

function usage(code) {
  process.stdout.write(`usage:
  jev-browse --plan plan.json|- [--session NAME] [--mode plan|step] [--step N] [--json]
  jev-browse [--session NAME] --do "click the Sign in button" [--value V] [--key K] [--url U]
  jev-browse [--session NAME] --expect "the dashboard is shown"
  jev-browse --close [SESSION]          shut down the session's live browser
options: --min-confidence X  --headed  --reset (forget the saved session first)
plan: {"url","goal","steps":[{"do","value"?,"key"?,"url"?,"action"?,"selector"?}|{"expect"}],
       "mode":"plan"|"step","minConfidence":0.9,"headless":true,"storageState"?,"maxSteps":30}
`);
  process.exit(code);
}

function fail(msg) {
  process.stderr.write(`jev-browse: ${msg}\n`);
  process.exit(2);
}

function loadPlan(args) {
  let plan;
  if (args.plan) {
    const raw = args.plan === '-' ? fs.readFileSync(0, 'utf8') : fs.readFileSync(args.plan, 'utf8');
    try { plan = JSON.parse(raw); } catch (e) { fail(`plan is not valid JSON: ${e.message}`); }
  } else if (args.do || args.expect || args.url) {
    const step = args.expect ? { expect: args.expect } : args.do ? { do: args.do } : null;
    if (step && args.value !== undefined) step.value = args.value;
    if (step && args.key) step.key = args.key;
    if (step && args.selector) step.selector = args.selector;
    if (step && args.action) step.action = args.action;
    plan = { mode: 'step', steps: step ? [step] : [] };
    if (args.url) { plan.url = args.url; plan.resume = false; }
  } else {
    usage(2);
  }
  if (!plan || typeof plan !== 'object') fail('plan must be a JSON object');
  plan.steps = Array.isArray(plan.steps) ? plan.steps : [];
  plan.steps.forEach((s, i) => {
    if (!s || (typeof s.do !== 'string' && typeof s.expect !== 'string')) {
      fail(`step ${i + 1} needs a "do" or "expect" string`);
    }
  });
  if (args.mode) plan.mode = args.mode;
  plan.mode = plan.mode === 'step' ? 'step' : 'plan';
  if (args.minConfidence) plan.minConfidence = args.minConfidence;
  plan.minConfidence = Number(plan.minConfidence) > 0 ? Number(plan.minConfidence) : 0.9;
  plan.maxSteps = Number(plan.maxSteps) > 0 ? Number(plan.maxSteps) : 30;
  if (args.headed) plan.headless = false;
  plan.headless = plan.headless !== false;
  return plan;
}

// ---------------------------------------------------------------- session

function sessionPaths(name) {
  if (!/^[A-Za-z0-9._-]{1,64}$/.test(name)) fail(`bad session name: ${name}`);
  const dir = path.join(HOME_DIR, name);
  return { dir, state: path.join(dir, 'state.json'), meta: path.join(dir, 'session.json') };
}

function readJSON(file, fallback) {
  try { return JSON.parse(fs.readFileSync(file, 'utf8')); } catch { return fallback; }
}

function logDecision(rec) {
  try {
    fs.mkdirSync(HOME_DIR, { recursive: true });
    fs.appendFileSync(DECISIONS, JSON.stringify({ ts: new Date().toISOString(), ...rec }) + '\n');
  } catch { /* logging must never break a run */ }
}

// ---------------------------------------------------------------- jev

function jevBinary() {
  if (process.env.JEV_BIN) return process.env.JEV_BIN;
  const local = path.join(HERE, '..', 'bin', 'jev');
  return fs.existsSync(local) ? local : 'jev';
}

const usage_ = { input_tokens: 0, output_tokens: 0, calls: 0 };

function jevDecide(request, attempt = 1) {
  return new Promise((resolve, reject) => {
    const child = spawn(jevBinary(), ['decide'], { stdio: ['pipe', 'pipe', 'pipe'] });
    let out = '';
    let err = '';
    const timer = setTimeout(() => child.kill('SIGKILL'), 90_000);
    child.stdout.on('data', (d) => { out += d; });
    child.stderr.on('data', (d) => { err += d; });
    child.on('error', (e) => { clearTimeout(timer); reject(new Error(`cannot run jev: ${e.message}`)); });
    child.on('close', (code) => {
      clearTimeout(timer);
      let parsed = null;
      try { parsed = JSON.parse(out); } catch { /* handled below */ }
      if (code === 0 && parsed && parsed.answers) {
        usage_.calls++;
        usage_.input_tokens += parsed.usage?.input_tokens || 0;
        usage_.output_tokens += parsed.usage?.output_tokens || 0;
        return resolve(parsed);
      }
      const why = `jev decide failed (exit ${code}): ${(err || out).trim().slice(0, 300)}`;
      if (attempt < 2) return resolve(jevDecide(request, attempt + 1));
      reject(new Error(why));
    });
    child.stdin.end(JSON.stringify(request));
  });
}

// Normalise a choice answer into {choice, confidence, ranked:[[key, p]...]}.
function readChoice(ans) {
  const probs = ans?.probabilities || {};
  const ranked = Object.entries(probs).sort((x, y) => y[1] - x[1]);
  const choice = ans?.choice || ranked[0]?.[0] || 'none';
  let confidence = Number(ans?.confidence);
  if (!Number.isFinite(confidence) || confidence <= 0) confidence = probs[choice] ?? 0;
  return { choice, confidence, ranked };
}

// ---------------------------------------------------------------- page snapshot

// Runs in the page. Tags visible interactive elements with data-jev-id and
// returns short descriptors, viewport elements first.
function snapshotInPage(max) {
  const SEL = 'a[href],button,input:not([type=hidden]),select,textarea,summary,[role],[contenteditable=""],[contenteditable=true],[tabindex]:not([tabindex="-1"])';
  const ROLES = new Set(['button', 'link', 'textbox', 'searchbox', 'checkbox', 'radio', 'switch', 'combobox',
    'listbox', 'option', 'menuitem', 'menuitemcheckbox', 'menuitemradio', 'tab', 'slider', 'spinbutton', 'treeitem', 'gridcell']);
  document.querySelectorAll('[data-jev-id]').forEach((el) => el.removeAttribute('data-jev-id'));
  const clean = (s, n = 60) => {
    s = (s || '').replace(/\s+/g, ' ').trim();
    return s.length > n ? s.slice(0, n - 1) + '…' : s;
  };
  const roleOf = (el) => {
    const r = el.getAttribute('role');
    if (r) return r.split(' ')[0];
    const tag = el.tagName.toLowerCase();
    if (tag === 'a') return 'link';
    if (tag === 'button' || tag === 'summary') return 'button';
    if (tag === 'select') return el.multiple ? 'listbox' : 'combobox';
    if (tag === 'textarea') return 'textbox';
    if (tag === 'input') {
      const t = (el.getAttribute('type') || 'text').toLowerCase();
      if (['button', 'submit', 'reset', 'image'].includes(t)) return 'button';
      if (t === 'checkbox' || t === 'radio') return t;
      if (t === 'range') return 'slider';
      if (t === 'number') return 'spinbutton';
      if (t === 'search') return 'searchbox';
      if (t === 'file') return 'file-input';
      return 'textbox';
    }
    if (el.isContentEditable) return 'textbox';
    return tag;
  };
  const nameOf = (el) => {
    const aria = el.getAttribute('aria-label');
    if (aria) return aria;
    const lb = el.getAttribute('aria-labelledby');
    if (lb) {
      const t = lb.split(/\s+/).map((id) => document.getElementById(id)?.innerText || '').join(' ');
      if (t.trim()) return t;
    }
    if (el.labels && el.labels.length) return Array.from(el.labels).map((l) => l.innerText).join(' ');
    const tag = el.tagName.toLowerCase();
    if (tag === 'input') {
      const t = (el.getAttribute('type') || '').toLowerCase();
      if (['button', 'submit', 'reset'].includes(t)) return el.value || t;
      if (t === 'image') return el.alt || 'image';
    }
    if (tag !== 'select' && tag !== 'input' && tag !== 'textarea') {
      const txt = el.innerText;
      if (txt && txt.trim()) return txt;
      const img = el.querySelector('img[alt],svg[aria-label]');
      if (img) return img.getAttribute('alt') || img.getAttribute('aria-label');
    }
    return el.getAttribute('title') || el.getAttribute('name') || el.id || '';
  };
  const visible = (el) => {
    const r = el.getBoundingClientRect();
    if (r.width < 1 || r.height < 1) return false;
    const cs = getComputedStyle(el);
    if (cs.visibility === 'hidden' || cs.display === 'none' || Number(cs.opacity) === 0) return false;
    if (el.closest('[aria-hidden="true"],[inert]')) return false;
    return true;
  };
  const vw = innerWidth;
  const vh = innerHeight;
  const out = [];
  const seen = new Set();
  for (const el of document.querySelectorAll(SEL)) {
    if (seen.has(el)) continue;
    seen.add(el);
    const role = roleOf(el);
    const tag = el.tagName.toLowerCase();
    const native = ['a', 'button', 'input', 'select', 'textarea', 'summary'].includes(tag) || el.isContentEditable;
    if (!native && !ROLES.has(role) && !el.hasAttribute('tabindex')) continue;
    if (!visible(el)) continue;
    const r = el.getBoundingClientRect();
    const inView = r.bottom > 0 && r.right > 0 && r.top < vh && r.left < vw;
    const extras = [];
    const ph = el.getAttribute('placeholder');
    if (ph) extras.push(`placeholder "${clean(ph, 40)}"`);
    if (tag === 'input') {
      const t = (el.getAttribute('type') || 'text').toLowerCase();
      if (!['text', 'submit', 'button', 'checkbox', 'radio'].includes(t)) extras.push(`type ${t}`);
      if ((t === 'checkbox' || t === 'radio') && el.checked) extras.push('checked');
      if (role === 'textbox' && el.value && t !== 'password') extras.push(`value "${clean(el.value, 30)}"`);
      if (t === 'password' && el.value) extras.push('filled');
    }
    if (tag === 'textarea' && el.value) extras.push('filled');
    if (tag === 'select') {
      const opts = Array.from(el.options).map((o) => clean(o.text, 20)).filter(Boolean);
      extras.push(`options: ${opts.slice(0, 6).join(' | ')}${opts.length > 6 ? ` (+${opts.length - 6})` : ''}`);
      if (el.selectedOptions[0]?.text) extras.push(`selected "${clean(el.selectedOptions[0].text, 20)}"`);
    }
    if (tag === 'a') {
      const href = el.getAttribute('href') || '';
      if (href && !href.startsWith('javascript:')) extras.push(`-> ${clean(href, 40)}`);
    }
    if (el.disabled || el.getAttribute('aria-disabled') === 'true') extras.push('disabled');
    const name = clean(nameOf(el));
    const desc = `${role} "${name}"${extras.length ? ' (' + extras.join(', ') + ')' : ''}`;
    out.push({ el, desc, short: `${role} "${clean(name, 40)}"`, role, tag, inView, top: r.top, left: r.left });
  }
  out.sort((a, b) => (a.inView === b.inView ? (a.top - b.top) || (a.left - b.left) : a.inView ? -1 : 1));
  const total = out.length;
  return {
    total,
    title: document.title,
    elements: out.slice(0, max).map((c, i) => {
      const id = `e${i + 1}`;
      c.el.setAttribute('data-jev-id', id);
      return { id, desc: c.desc, short: c.short, role: c.role, tag: c.tag, inView: c.inView };
    }),
  };
}

function pageTextInPage(limit) {
  const text = (document.body?.innerText || '').replace(/[ \t]+/g, ' ').replace(/\n\s*\n+/g, '\n').trim();
  return { title: document.title, text: text.slice(0, limit) };
}

// ---------------------------------------------------------------- step execution

const TEXT_ROLES = new Set(['textbox', 'searchbox', 'spinbutton', 'combobox']);
const KEY_NAMES = {
  enter: 'Enter', return: 'Enter', tab: 'Tab', escape: 'Escape', esc: 'Escape', space: 'Space',
  backspace: 'Backspace', delete: 'Delete', up: 'ArrowUp', down: 'ArrowDown', left: 'ArrowLeft', right: 'ArrowRight',
  arrowup: 'ArrowUp', arrowdown: 'ArrowDown', arrowleft: 'ArrowLeft', arrowright: 'ArrowRight', home: 'Home', end: 'End',
  pageup: 'PageUp', pagedown: 'PageDown',
};

function keyFromStep(step) {
  if (step.key) return step.key;
  const m = /\bpress(?:es|ing)?\s+(?:the\s+)?([A-Za-z0-9+]+)/i.exec(step.do || '');
  if (!m) return null;
  const k = m[1];
  if (k.includes('+')) return k;
  return KEY_NAMES[k.toLowerCase()] || (k.length === 1 ? k : null);
}

function urlFromStep(step, base) {
  const raw = step.url || /\b(https?:\/\/\S+|file:\/\/\S+)/i.exec(step.do || '')?.[1];
  if (!raw) return null;
  try { return new URL(raw.replace(/[.,)"']+$/, ''), base).href; } catch { return null; }
}

async function settle(page) {
  await page.waitForLoadState('domcontentloaded', { timeout: 10_000 }).catch(() => {});
  await page.waitForLoadState('networkidle', { timeout: 2_500 }).catch(() => {});
}

class Runner {
  constructor(plan, ctx, page, session) {
    this.plan = plan;
    this.ctx = ctx;
    this.page = page;
    this.session = session;
    this.log = [];
  }

  // Follow a tab the last action opened.
  adoptNewestPage() {
    const pages = this.ctx.pages().filter((p) => !p.isClosed());
    if (pages.length) this.page = pages[pages.length - 1];
  }

  async runStep(step, n) {
    const minC = Number(step.minConfidence) > 0 ? Number(step.minConfidence) : this.plan.minConfidence;
    return step.expect !== undefined ? this.runExpect(step, n, minC) : this.runDo(step, n, minC);
  }

  async runExpect(step, n, minC) {
    const page = this.page;
    const { title, text } = await page.evaluate(pageTextInPage, PAGE_TEXT_CHARS);
    const res = await jevDecide({
      state: { url: page.url(), title, visible_text: text },
      questions: {
        expect: {
          type: 'noul',
          instructions: `This is the current state of a web page in a browser. Is the following statement true of this page right now? "${step.expect}"`,
        },
      },
    });
    const p = Number(res.answers?.expect?.noul ?? 0);
    const passed = p >= minC;
    const failed = p <= 1 - minC;
    const outcome = passed ? 'pass' : failed ? 'fail' : 'unsure';
    const entry = { step: n, expect: step.expect, element: null, action: 'expect', confidence: round(p), outcome };
    this.log.push(entry);
    logDecision({ session: this.session, step: n, kind: 'expect', text: step.expect, chosen: outcome, confidence: round(p), executed: true, outcome });
    if (passed) return { ok: true, entry };
    return {
      ok: false,
      entry,
      handback: {
        reason: failed ? `expectation failed (p=${fmt(p)})` : `not sure the expectation holds (p=${fmt(p)}, need ${minC})`,
        step: n,
        stepText: { expect: step.expect },
        pageText: text.slice(0, 400),
      },
    };
  }

  async runDo(step, n, minC) {
    let page = this.page;
    const snap = await page.evaluate(snapshotInPage, MAX_CANDIDATES);
    const byId = new Map(snap.elements.map((e) => [e.id, e]));

    // Ask only what the plan does not already pin down.
    const questions = {};
    if (!step.selector) {
      const criteria = {};
      for (const e of snap.elements) criteria[e.id] = e.desc;
      criteria.none = 'none of the listed elements (no match), or the step is ambiguous between several elements';
      questions.element = {
        type: 'choice',
        instructions: `Which one element on this web page should the step "${step.do}" be performed on?`,
        criteria,
      };
      // A single-pass choice collapses ambiguity onto the likeliest element
      // ("click the button" -> the submit button at 0.96), so ask separately
      // how many elements the wording fits and require "one".
      questions.count = {
        type: 'choice',
        instructions: `Given the list of page elements, how many of them could the step "${step.do}" plausibly refer to? Count every element the wording fits, not just the most likely one.`,
        criteria: {
          zero: 'no element fits',
          one: 'exactly one element fits; the step clearly names it',
          several: 'two or more elements fit the wording',
        },
      };
    }
    if (!step.action) {
      questions.action = {
        type: 'choice',
        instructions: `A browser automation step says: "${step.do}". What kind of browser action is this?`,
        criteria: ACTIONS,
      };
    }

    let el = { choice: null, confidence: 1, ranked: [] };
    let act = { choice: step.action, confidence: 1, ranked: [] };
    let count = { choice: 'one', confidence: 1, ranked: [] };
    if (Object.keys(questions).length) {
      const res = await jevDecide({
        state: {
          url: page.url(), title: snap.title, goal: this.plan.goal || '', step: step.do,
          elements: snap.elements.map((e) => `${e.id}: ${e.desc}`).join('\n'),
        },
        questions,
      });
      if (questions.element) el = readChoice(res.answers?.element);
      if (questions.action) act = readChoice(res.answers?.action);
      if (questions.count) count = readChoice(res.answers?.count);
    }

    let action = act.choice;
    // "open the Pricing page" reads as navigate, but with no URL in the step it
    // means following a link: treat it as a click on the chosen element.
    if (action === 'navigate' && !urlFromStep(step, page.url()) && byId.get(el.choice)) action = 'click';
    const needsElement = action === 'click' || action === 'fill' || action === 'select';
    const target = step.selector ? { id: 'selector', desc: `selector ${step.selector}`, short: step.selector } : byId.get(el.choice) || null;
    const entry = {
      step: n,
      do: step.do,
      element: target ? `${target.id} ${target.short}` : null,
      action,
      confidence: { element: round(el.confidence), action: round(act.confidence), single: round(count.ranked.find(([k]) => k === 'one')?.[1] ?? 1) },
      outcome: 'pending',
    };
    this.log.push(entry);

    const handback = (reason) => {
      entry.outcome = 'handback';
      logDecision({ session: this.session, step: n, kind: 'do', text: step.do, chosen: { element: entry.element, action }, confidence: entry.confidence, executed: false, outcome: reason });
      return {
        ok: false,
        entry,
        handback: {
          reason,
          step: n,
          stepText: step,
          candidates: el.ranked.slice(0, 3).map(([id, p]) => ({ id, p: round(p), desc: id === 'none' ? 'none' : byId.get(id)?.desc })),
          actions: act.ranked.slice(0, 3).map(([k, p]) => ({ action: k, p: round(p) })),
          elements: snap.elements.slice(0, HANDBACK_ELEMENTS).map((e) => `${e.id} ${e.desc}${e.inView ? '' : ' [offscreen]'}`),
          totalElements: snap.total,
        },
      };
    };

    // Confidence gates.
    if (!action || action === 'none') return handback('could not tell which action this step is');
    if (act.confidence < minC) return handback(`unsure which action (${act.choice} ${fmt(act.confidence)} < ${minC})`);
    if (needsElement && !step.selector) {
      if (!target) return handback('no single element matches this step');
      if (el.confidence < minC) return handback(`unsure which element (${target.id} ${fmt(el.confidence)} < ${minC})`);
      if (entry.confidence.single < minC) {
        return handback(`step may fit several elements (p(exactly one)=${fmt(entry.confidence.single)} < ${minC})`);
      }
    }

    // Values come from the plan only.
    let value = null;
    if (action === 'fill' || action === 'select') {
      if (step.value === undefined || step.value === null) return handback(`step needs a "value" for ${action}`);
      value = String(step.value);
    }
    if (target && !step.selector) {
      if (action === 'fill' && !TEXT_ROLES.has(target.role) && target.tag !== 'textarea' && target.role !== 'textbox') {
        return handback(`chose ${action} on a ${target.role}, which cannot take text`);
      }
      if (action === 'select' && target.tag !== 'select' && !['combobox', 'listbox'].includes(target.role)) {
        return handback(`chose select on a ${target.role}, which is not a dropdown`);
      }
    }
    let key = null;
    if (action === 'press') {
      key = keyFromStep(step);
      if (!key) return handback('step says press but names no key (add "key")');
    }
    let navUrl = null;
    if (action === 'navigate') {
      navUrl = urlFromStep(step, page.url());
      if (!navUrl) return handback('step says navigate but has no URL (add "url")');
    }

    // Execute.
    const locator = step.selector ? page.locator(step.selector).first()
      : target ? page.locator(`[data-jev-id="${target.id}"]`) : null;
    const pagesBefore = this.ctx.pages().length;
    try {
      if (action === 'click') {
        await locator.click({ timeout: ACTION_TIMEOUT });
      } else if (action === 'fill') {
        await locator.fill(value, { timeout: ACTION_TIMEOUT });
      } else if (action === 'select') {
        const tag = await locator.evaluate((e) => e.tagName.toLowerCase(), null, { timeout: ACTION_TIMEOUT });
        if (tag === 'select') {
          const r = await locator.selectOption({ label: value }, { timeout: ACTION_TIMEOUT }).catch(() => null);
          if (!r || !r.length) await locator.selectOption(value, { timeout: ACTION_TIMEOUT });
        } else {
          // Custom dropdown: open it, then click the option by its text.
          await locator.click({ timeout: ACTION_TIMEOUT });
          await page.getByRole('option', { name: value }).or(page.getByText(value, { exact: true })).first().click({ timeout: ACTION_TIMEOUT });
        }
      } else if (action === 'press') {
        if (locator) await locator.press(key, { timeout: ACTION_TIMEOUT });
        else await page.keyboard.press(key);
      } else if (action === 'navigate') {
        await page.goto(navUrl, { timeout: 30_000 });
      }
    } catch (e) {
      const msg = String(e.message || e).split('\n')[0].slice(0, 200);
      return handback(`action failed: ${msg}`);
    }
    await settle(page);
    if (this.ctx.pages().length > pagesBefore) {
      this.adoptNewestPage();
      await settle(this.page);
    }
    entry.outcome = 'ok';
    logDecision({ session: this.session, step: n, kind: 'do', text: step.do, chosen: { element: entry.element, action }, confidence: entry.confidence, executed: true, outcome: 'ok' });
    return { ok: true, entry };
  }
}

const round = (x) => Math.round(Number(x || 0) * 1000) / 1000;
const fmt = (x) => Number(x || 0).toFixed(2);
const tilde = (p) => (p && p.startsWith(os.homedir()) ? '~' + p.slice(os.homedir().length) : p);

// ---------------------------------------------------------------- main

// A live browser per session keeps in-page state (typed text, open menus,
// sessionStorage) between invocations, which step mode needs. It runs in a
// detached `--serve` process that exits after JEV_BROWSE_IDLE_MIN minutes
// (default 15) without use. storageState + URL in the session dir are the
// fallback when that browser is gone, so cookies and location survive anyway.

const IDLE_MS = (Number(process.env.JEV_BROWSE_IDLE_MIN) || 15) * 60_000;
const VIEWPORT = { width: 1280, height: 900 };

async function serve(sessionName, headless) {
  const sp = sessionPaths(sessionName);
  const profile = path.join(sp.dir, 'profile');
  fs.mkdirSync(profile, { recursive: true });
  try { fs.unlinkSync(path.join(profile, 'DevToolsActivePort')); } catch { /* none */ }
  const ctx = await chromium.launchPersistentContext(profile, {
    headless, viewport: VIEWPORT, args: ['--remote-debugging-port=0', '--remote-debugging-address=127.0.0.1'],
  });
  const saved = readJSON(sp.state, null);
  if (saved?.cookies?.length) await ctx.addCookies(saved.cookies).catch(() => {});
  let port = null;
  for (let i = 0; i < 100 && !port; i++) {
    try { port = fs.readFileSync(path.join(profile, 'DevToolsActivePort'), 'utf8').split('\n')[0].trim(); } catch { /* not yet */ }
    if (!port) await new Promise((r) => setTimeout(r, 100));
  }
  const live = path.join(sp.dir, 'browser.json');
  const shutdown = async () => {
    try { if (readJSON(live, {}).pid === process.pid) fs.unlinkSync(live); } catch { /* gone */ }
    await ctx.close().catch(() => {});
    process.exit(0);
  };
  if (!port) await shutdown();
  fs.writeFileSync(live, JSON.stringify({ pid: process.pid, endpoint: `http://127.0.0.1:${port}`, headless, started: new Date().toISOString() }));
  ctx.on('close', () => shutdown());
  process.on('SIGTERM', shutdown);
  process.on('SIGINT', shutdown);
  setInterval(() => {
    let last = 0;
    try { last = fs.statSync(sp.meta).mtimeMs; } catch { /* never used */ }
    try { last = Math.max(last, fs.statSync(live).mtimeMs); } catch { return shutdown(); }
    if (Date.now() - last > IDLE_MS) shutdown();
  }, 20_000);
}

function alive(pid) {
  try { process.kill(pid, 0); return true; } catch { return false; }
}

async function connectLive(sp) {
  const info = readJSON(path.join(sp.dir, 'browser.json'), null);
  if (!info || !alive(info.pid)) return null;
  try {
    const browser = await chromium.connectOverCDP(info.endpoint, { timeout: 5_000 });
    const ctx = browser.contexts()[0];
    if (!ctx) { await browser.close().catch(() => {}); return null; }
    return { browser, ctx, info };
  } catch { return null; }
}

async function startLive(sp, sessionName, headless) {
  const child = spawn(process.execPath, [fileURLToPath(import.meta.url), '--serve', sessionName, ...(headless ? [] : ['--headed'])], {
    detached: true, stdio: 'ignore',
  });
  child.unref();
  for (let i = 0; i < 150; i++) {
    await new Promise((r) => setTimeout(r, 100));
    const info = readJSON(path.join(sp.dir, 'browser.json'), null);
    if (info && info.pid === child.pid) return connectLive(sp);
  }
  return null;
}

async function stopLive(sp) {
  const info = readJSON(path.join(sp.dir, 'browser.json'), null);
  if (info && alive(info.pid)) {
    process.kill(info.pid, 'SIGTERM');
    for (let i = 0; i < 50 && alive(info.pid); i++) await new Promise((r) => setTimeout(r, 100));
  }
  try { fs.unlinkSync(path.join(sp.dir, 'browser.json')); } catch { /* gone */ }
}

// Returns {ctx, page, fresh, close}. fresh = the page has no state to resume.
async function openBrowser(plan, sp, sessionName, wantLive) {
  if (wantLive) {
    let live = await connectLive(sp);
    if (live && live.info.headless !== plan.headless) { await live.browser.close().catch(() => {}); await stopLive(sp); live = null; }
    if (!live) live = await startLive(sp, sessionName, plan.headless);
    if (live) {
      const pages = live.ctx.pages().filter((p) => !p.isClosed());
      const used = pages.filter((p) => p.url() !== 'about:blank');
      const page = used[used.length - 1] || pages[0] || await live.ctx.newPage();
      for (const p of pages) if (p !== page && p.url() === 'about:blank') await p.close().catch(() => {});
      fs.utimesSync(path.join(sp.dir, 'browser.json'), new Date(), new Date());
      // Disconnecting from a CDP browser leaves it and its default context running.
      return { ctx: live.ctx, page, fresh: page.url() === 'about:blank', live: true, close: () => live.browser.close() };
    }
  }
  const browser = await chromium.launch({ headless: plan.headless });
  const storage = plan.storageState && fs.existsSync(plan.storageState) ? plan.storageState
    : fs.existsSync(sp.state) ? sp.state : undefined;
  const ctx = await browser.newContext({ storageState: storage, viewport: VIEWPORT });
  return { ctx, page: await ctx.newPage(), fresh: true, live: false, close: () => browser.close() };
}

async function main() {
  const argv = process.argv.slice(2);
  if (argv[0] === '--serve') return serve(argv[1], !argv.includes('--headed'));
  if (argv[0] === '--close') {
    const sp = sessionPaths(argv[1] || 'default');
    await stopLive(sp);
    process.stdout.write(`closed browser for session ${argv[1] || 'default'}\n`);
    return;
  }
  const args = parseArgs(argv);
  const plan = loadPlan(args);
  const sessionName = args.session || plan.session || 'default';
  const sp = sessionPaths(sessionName);
  if (args.reset) {
    await stopLive(sp);
    fs.rmSync(sp.dir, { recursive: true, force: true });
  }
  fs.mkdirSync(sp.dir, { recursive: true });
  const meta = readJSON(sp.meta, { url: null, stepCount: 0 });
  const wantLive = plan.live ?? process.env.JEV_BROWSE_LIVE !== '0';

  // Which steps run this time.
  let steps = plan.steps.map((s, i) => ({ s, n: i + 1 }));
  if (args.step) steps = steps.filter((x) => x.n === args.step);
  if (plan.mode === 'step') steps = steps.slice(0, 1);
  const skipped = plan.mode === 'step' ? plan.steps.length - steps.length : 0;
  if (steps.length > plan.maxSteps) steps = steps.slice(0, plan.maxSteps);

  const result = { status: 'done', session: sessionName, mode: plan.mode, stepsRun: 0, log: [], url: null, screenshot: null };
  let b;
  try {
    b = await openBrowser(plan, sp, sessionName, wantLive);
  } catch (e) {
    const msg = String(e.message || e);
    result.status = 'error';
    result.error = /Executable doesn't exist/.test(msg)
      ? 'Chromium is not installed: run `npx playwright install chromium` in the plugin browse/ dir'
      : msg.split('\n')[0];
    return output(args, result);
  }

  try {
    let page = b.page;
    // Plan mode starts at plan.url; step mode (or a plan without url) continues
    // where the session left off. "resume" in the plan overrides either default.
    const resume = plan.resume ?? (plan.mode === 'step' || !plan.url);
    if (resume && !b.fresh) {
      // The live page is already where we left it.
    } else {
      const start = resume && meta.url ? meta.url : plan.url || meta.url;
      if (!start) throw new Error('no URL: give "url" in the plan (or --url) for a new session');
      await page.goto(start, { timeout: 30_000 });
    }
    await settle(page);

    const runner = new Runner(plan, b.ctx, page, sessionName);
    let stepNo = meta.stepCount || 0;
    for (const { s, n } of steps) {
      stepNo++;
      const r = await runner.runStep(s, n);
      result.stepsRun++;
      if (!r.ok) {
        result.status = 'handback';
        result.handback = r.handback;
        break;
      }
    }
    page = runner.page;
    result.log = runner.log;
    result.url = page.url();
    result.screenshot = path.join(sp.dir, `step-${stepNo}.png`);
    await page.screenshot({ path: result.screenshot }).catch(() => { result.screenshot = null; });

    meta.url = result.url;
    meta.stepCount = stepNo;
    meta.updated = new Date().toISOString();
    await b.ctx.storageState({ path: sp.state });
    if (plan.storageState) await b.ctx.storageState({ path: plan.storageState }).catch(() => {});
    fs.writeFileSync(sp.meta, JSON.stringify(meta, null, 2));
    if (skipped > 0) result.remaining = skipped;
  } catch (e) {
    result.status = 'error';
    result.error = String(e.message || e).split('\n')[0].slice(0, 300);
  } finally {
    await b.close().catch(() => {});
  }
  result.usage = usage_;
  return output(args, result);
}

function output(args, r) {
  if (args.json) {
    process.stdout.write(JSON.stringify(r) + '\n');
  } else {
    const lines = [];
    for (const e of r.log) {
      if (e.action === 'expect') {
        lines.push(`${e.outcome === 'pass' ? 'ok ' : '!! '} ${e.step} expect "${e.expect}" -> ${e.outcome} (p ${fmt(e.confidence)})`);
      } else {
        const tag = e.outcome === 'ok' ? 'ok ' : '!! ';
        lines.push(`${tag} ${e.step} ${e.action || '?'} ${e.element || '-'} (element ${fmt(e.confidence.element)}, single ${fmt(e.confidence.single)}, action ${fmt(e.confidence.action)})`);
      }
    }
    if (r.status === 'handback') {
      const h = r.handback;
      lines.push(`HANDBACK at step ${h.step}: ${h.reason}`);
      lines.push(`  step: ${JSON.stringify(h.stepText)}`);
      if (h.candidates?.length) {
        lines.push('  top candidates: ' + h.candidates.map((c) => `${c.id} ${fmt(c.p)}${c.desc && c.id !== 'none' ? ' ' + c.desc : ''}`).join('; '));
      }
      if (h.actions?.length) lines.push('  actions: ' + h.actions.map((a) => `${a.action} ${fmt(a.p)}`).join(', '));
      if (h.elements?.length) {
        lines.push(`  elements (${h.elements.length} of ${h.totalElements}):`);
        for (const e of h.elements) lines.push('    ' + e);
      }
      if (h.pageText !== undefined) lines.push(`  page text: ${JSON.stringify(h.pageText.slice(0, 300))}`);
    }
    if (r.status === 'error') lines.push(`ERROR: ${r.error}`);
    const tail = [`${r.status} ${r.stepsRun} step(s)`];
    if (r.remaining) tail.push(`${r.remaining} not run (step mode)`);
    if (r.url) tail.push(r.url);
    lines.push(tail.join(' | '));
    if (r.screenshot) lines.push(`screenshot: ${tilde(r.screenshot)}`);
    process.stdout.write(lines.join('\n') + '\n');
  }
  process.exitCode = r.status === 'done' ? 0 : r.status === 'handback' ? 3 : 1;
}

main().catch((e) => {
  process.stderr.write(`jev-browse: ${e.stack || e}\n`);
  process.exit(1);
});
