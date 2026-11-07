// The panel: one surface, three tabs, floating over the transcript instead of squeezing it.
//
// This is the debugging surface for the claim that bareloop has no hidden behaviour — so it shows the
// system prompt verbatim, every message the assembler kept, which ones were summarised away, the exact
// request body behind them, what the window is full of and what it costs, and which tools a request
// would carry. It used to be three panels with three doors, and opening one relaid out the transcript
// (a grid column appearing is a reflow); the panel now floats, so the answer to "what is in the
// request" never moves the conversation you are reading.
//
// The three tabs share one mode variable, so a module can ask "am I open?" without watching the DOM,
// and opening one closes the others by construction rather than by remembering to.
import { $, el, fmtN } from './util.js';
import { icon } from './icons.js';
import { state } from './state.js';
import { get } from './api.js';
import { note } from './ui.js';
import { compactNow } from './app.js';
import { EV_REFRESH, EV_SESSION, on } from './bus.js';

// Which tab is on screen ('' = the panel is closed).
let mode = '';

// renderers is each tab's draw queue, registered by the modules that own the content. They register
// rather than being imported here because panel.js is imported by them — a direct import would close
// the circle, and an ESM circle resolves its way through the module that loaded first, which is a fact
// about loaders that no reader can check from here.
//
// A name holds a queue, not one function: the context tab is two owners answering one question — the
// window's arithmetic (here) and its bill (stats.js) — drawn into the same body, in registration order,
// with a rule between what they each fetched.
const renderers = {};
export function registerRenderer(name, fn) {
  (renderers[name] = renderers[name] || []).push(fn);
}

export const panelMode = () => mode;

// Registered at module scope and not inside wirePanel: app.js imports panel.js before stats.js, so
// this module's context renderer is in the queue first and the bill lands after it. Registration
// from a wire function would run after every module top-level, which silently inverts the tab.
registerRenderer('inspect', renderInspect);
registerRenderer('context', renderContext);

// openPanel marks the tab, reveals the surface, and hands back the cleared body to fill.
//
// The ticket is why this is not just a classList.toggle: render queues are async, so the writes of a
// queue started for a previous tab can still be in flight when the reader clicks on. A late append
// would land in the new tab's body under the new tab's name — a bill drawn under the heading of tools.
// Only the queue holding the newest ticket may draw; the old one's next append is its last.
let ticket = 0;

// bindAll walks an object one level and returns it with every method (and every method of a
// style/attributes/dataset bag) bound to the real element, so the object may be used as `this` in
// native calls (`el.append(a, this)` in tools.js) without tripping the "Illegal invocation" that a
// Proxy receiver raises against a native method. The point of the walk is exactly what it forbids:
// an UNbound method could be handed to someone else and called later, after the ticket expired,
// drawing into this body under a different tab's name — those stay bound to the guard, and their
// next call throws.
function bindAll(guard, real) {
  const out = Object.create(null);
  const bindOne = (v) => (typeof v === 'function' ? v.bind(real) : v);
  for (const k of Object.keys(real)) out[k] = bindOne(real[k]);
  for (const k of ['style', 'dataset', 'attributes', 'classList', 'dataset']) {
    const bag = real[k];
    if (!bag || typeof bag !== 'object') continue;
    const sub = Object.create(null);
    for (const kk of Object.keys(bag)) sub[kk] = bindOne(bag[kk]);
    out[k] = new Proxy(sub, {
      get: (t, p2, r2) => {
        if (ticket !== guard.id) throw new Error('panel moved on');
        return Reflect.get(bag, p2, r2);
      },
      set: (t, p2, v2) => {
        if (ticket !== guard.id) throw new Error('panel moved on');
        bag[p2] = v2;
        return true;
      },
      has: () => true,
    });
  }
  return out;
}

export function openPanel(next) {
  mode = next;
  const mine = ++ticket;
  $('#panel').classList.remove('hidden');
  for (const b of document.querySelectorAll('#panel [data-tab]')) {
    const on = b.dataset.tab === next;
    b.classList.toggle('on', on);
    b.setAttribute('aria-selected', on ? 'true' : 'false');
  }
  const body = $('#panel-body');
  const guard = {
    id: mine,
    get childNodes() {
      if (ticket !== mine) throw new Error('panel moved on');
      return body.childNodes;
    },
  };
  const bound = bindAll(guard, body);
  const wrapped = new Proxy(guard, {
    get(target, prop, recv) {
      if (ticket !== mine) throw new Error('panel moved on');
      if (prop === 'raw') return body; // the escape hatch renderers use to pass themselves on
      if (prop in bound) {
        const v = Reflect.get(bound, prop);
        return typeof v === 'function' ? v.bind(body) : v;
      }
      const v = Reflect.get(body, prop, body);
      return typeof v === 'function' ? v.bind(body) : v;
    },
    set(target, prop, v) {
      if (ticket !== mine) throw new Error('panel moved on');
      body[prop] = v;
      return true;
    },
    has: () => true,
  });
  body.textContent = '';
  return wrapped;
}

// showPanelTab is what a click, a keypress or a ring gets: open the tab and run its render queue. A
// renderer that throws (a 500 from /inspect) leaves the panel open on what came before it rather than
// swallowing the click — api.js has already reported the failure itself.
export function showPanelTab(next) {
  const body = openPanel(next);
  const queue = (renderers[next] || []).slice();
  (async () => {
    for (const fn of queue) {
      // The rule goes up only if something actually drew: a renderer that failed and left nothing
      // behind should not be separated from its successor by a line over an empty gap.
      if (body.childNodes.length) body.appendChild(el('hr', 'tab-sep'));
      await fn(body);
    }
  })().catch((err) => {
    // A renderer's own failure was already reported by api.js; a queue that died mid-flight has not
    // been, and an empty panel with no trace is the worst failure this surface can have.
    if (!String(err?.message || '').includes('panel moved on')) console.warn('panel queue:', err);
  });
}

export function hidePanel() {
  mode = '';
  $('#panel').classList.add('hidden');
}

export function wirePanel() {
  $('#btn-inspect').onclick = () => showPanelTab('inspect');
  $('#panel-close').onclick = hidePanel;
  for (const b of document.querySelectorAll('#panel [data-tab]')) {
    b.onclick = () => showPanelTab(b.dataset.tab);
  }
  // The ring is the same fact the context tab spells out, at icon size — so it opens that tab.
  const ring = $('#ctx-ring');
  ring.onclick = () => showPanelTab('context');
  ring.onkeydown = (e) => {
    if (e.key === 'Enter' || e.key === ' ') {
      e.preventDefault();
      showPanelTab('context');
    }
  };
  // A gauge that stops moving while the thing it measures moves is a worse lie than no gauge: the live
  // tabs redraw on every refresh, and a session switch redraws whatever is open (the tools tab redraws
  // itself on EV_SESSION — it has its own view to ask the server for).
  on(EV_REFRESH, () => {
    if (mode === 'context') showPanelTab(mode);
  });
  on(EV_SESSION, () => {
    if (mode && mode !== 'tools') showPanelTab(mode);
  });
}

// ------------------------------------------------------------ the context tab

// The context tab: what is filling the window, what the next compaction would do about it, and the
// button that asks for one now. It is the first tab because it is the question the ring was pressed
// for, and compact belongs beside the numbers that motivate it rather than one strip away.
async function renderContext(body) {
  if (!state.id) {
    body.appendChild(el('div', 'dim', 'open a session first'));
    return;
  }
  const data = await get('/inspect').catch(() => null);
  if (!data) return;
  const rows = data.messages || [];
  body.appendChild(
    el(
      'div',
      'ctx-head',
      data.contextKnown === false
        ? 'no real measurement ' + (data.summary ? 'since the last compaction' : 'yet') + ' — the figure the ring shows is priced from the last request that actually ran'
        : `${fmtN(data.contextUsed)} of ${fmtN(data.nCtx)} tokens${data.estimated ? ' (estimated: the provider gives no per-request usage)' : ''}`,
    ),
  );
  body.appendChild(
    el(
      'div',
      'dim',
      `budget ${fmtN(data.budget)} · summary cap ${fmtN(data.summaryCap)} · ${data.systemBytes} bytes of system prompt · ${data.totalBytes} bytes total${data.syntheticToolResults ? ` · ${data.syntheticToolResults} synthetic tool result(s)` : ''}`,
    ),
  );
  if (data.boundaryId) body.appendChild(el('div', 'dim', `boundary ${data.boundaryId}${data.boundaryBroken ? ' (missing!)' : ''}`));
  // What the next compaction would do, planned by the server from the same entries it would send.
  // Shown when it succeeds as well as when it refuses: "what would a compaction here cost me" and "why
  // did nothing happen" are the same question, and this is the only place it can be asked first.
  if (data.plan) body.appendChild(el('div', 'ctx-plan', planLine(data.plan, rows.length)));
  if (data.tools?.length) body.appendChild(el('div', 'dim', `tools: ${data.tools.join(', ')}`));

  const actions = el('div', 'ctx-actions');
  const btn = el('button', 'inline-icon btn-warn', '');
  btn.appendChild(icon('compact', 'ico'));
  btn.appendChild(el('span', '', 'compact now'));
  btn.title = 'fold the oldest turns into a summary — the window frees up, the transcript keeps every entry';
  btn.onclick = () => compactNow();
  actions.appendChild(btn);
  actions.appendChild(el('span', 'dim', `auto-compaction fires past ${state.status?.thresholdPct ?? '?'}%`));
  body.appendChild(actions);
  body.appendChild(
    el(
      'div',
      'dim',
      'compact here or in Settings · the “inspect” tab shows exactly which messages a request carries',
    ),
  );
}

// ------------------------------------------------------------ the inspect tab

async function renderInspect(body) {
  if (!state.id) {
    body.appendChild(el('div', 'dim', 'open a session first'));
    return;
  }
  const data = await get('/inspect').catch(() => null);
  if (!data) return;
  const rows = data.messages || [];

  // The system prompt is part of the payload, so it belongs in this view. Without it there is no way
  // to see what steers the model — including which language and script it should answer in. Older
  // servers report only systemBytes, so the block is skipped rather than lying about "(none)".
  if (typeof data.system === 'string') {
    body.appendChild(el('div', 'lbl', 'system prompt (first message, sent verbatim)'));
    body.appendChild(
      el('pre', '', data.system || 'none: no system message is sent, the model falls back to its own defaults (its trained language and script included)'),
    );
  }

  body.appendChild(el('div', 'lbl', `${rows.length} messages would be sent`));
  const table = el('table');
  const tr = el('tr');
  for (const h of ['id', 'role', 'tokens', 'head']) tr.appendChild(el('th', '', h));
  table.appendChild(tr);
  for (const r of rows) {
    const row = el('tr');
    row.append(el('td', '', r.id || '(synthetic)'), el('td', '', r.role), el('td', 'num', fmtN(r.tokens)), el('td', 'head', r.head || ''));
    // Summarised away, or a synthetic empty tool result the assembler had to invent.
    if (!r.inKept) row.style.opacity = 0.45;
    if (!r.real) row.style.fontStyle = 'italic';
    table.appendChild(row);
  }
  body.appendChild(table);

  const actions = el('div', 'row');
  const raw = el('pre', 'payload hidden', '');
  const toggle = el('button', '', 'payload as sent');
  toggle.onclick = async () => {
    if (!raw.classList.contains('hidden')) {
      raw.classList.add('hidden');
      toggle.textContent = 'payload as sent';
      return;
    }
    toggle.textContent = 'reading…';
    const p = await get('/payload').catch(() => null);
    toggle.textContent = 'payload as sent';
    if (!p) return;
    if (!Array.isArray(p.messages)) {
      // An older server answers this path with the SPA shell, so check the shape before printing it.
      note('this server does not serve /payload yet — rebuild bareloopd', 'error');
      return;
    }
    raw.textContent = JSON.stringify(p, null, 2);
    raw.classList.remove('hidden');
    toggle.textContent = 'hide payload';
  };
  actions.append(toggle, el('span', 'dim', `or GET /api/session/${state.id}/payload`));
  body.appendChild(actions);
  body.appendChild(raw);
  body.appendChild(el('div', 'dim', 'dimmed rows are summarised away; italic tokens are byte/4 estimates'));
}

// planLine spells out the cut: how much of the path becomes a summary, what stays raw, and where the
// boundary lands.
//
// why the fields are capitalised: compact.Plan carries no json tags, so /inspect serialises Go's own
// field names — reading plan.cut or plan.keptTokens quietly gives undefined and the line prints zeros.
//
// why the numbers are only read when there is no reason: a refused plan is returned with just its
// reason filled in, so its cut and its budget are unset zeros rather than measurements.
function planLine(p, total) {
  if (p.Reason) return `next compaction: ${p.Reason}`;
  const boundary = p.Empty
    ? 'no boundary, the whole path becomes the summary'
    : `boundary ${p.BoundaryID}${p.Split ? ' (split inside a user turn)' : ''}`;
  // why the contradiction is spelled out instead of smoothed over: the tail is the greater of the budget
  // and min kept messages, so a plan may keep more than the budget two lines above reports, and the
  // reader has to be told which of the two won (§2.1).
  const over = p.KeptTokens > p.Budget ? ` — over the ${fmtN(p.Budget)}-token tail budget: min kept messages wins` : '';
  return `next compaction: ${p.Replaced} of ${total} messages \u2192 ${fmtN(p.SummarizeTokens)} tokens of summary, ${fmtN(
    p.KeptTokens,
  )} kept raw (${p.KeptUserTurns} user turns), ${boundary}${over}`;
}
