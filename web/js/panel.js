// The inspect panel: what would be sent next, and why.
//
// This is the debugging surface for the claim that bareloop has no hidden behaviour — so it shows the
// system prompt verbatim, every message the assembler kept, which ones were summarised away, and the
// exact request body behind them.
import { $, el, fmtN } from './util.js';
import { state } from './state.js';
import { get } from './api.js';
import { note } from './ui.js';

export function wirePanel() {
  $('#btn-inspect').onclick = showInspect;
  $('#panel-close').onclick = hidePanel;
}

// The panel has three modes: `inspect` (what would be sent), `tools` (the tool catalogue) and `stats`
// (tokens and cost of this conversation).
// One variable owns which one is on screen, so a module can ask “am I open?” without watching the DOM,
// and opening one closes the other by construction rather than by remembering to.
let mode = '';

export const panelMode = () => mode;

// openPanel shows the panel in one of its modes and hands back the body to fill.
export function openPanel(next, title) {
  mode = next;
  const app = $('#app');
  $('#panel').classList.remove('hidden');
  app.classList.add('with-panel');
  // The tool catalogue and the cost table need more width than a table of message ids, so the grid
  // follows the mode.
  app.classList.toggle('tools', next === 'tools');
  app.classList.toggle('stats', next === 'stats');
  $('#panel-title').textContent = title;
  const body = $('#panel-body');
  body.textContent = '';
  return body;
}

export function hidePanel() {
  mode = '';
  $('#panel').classList.add('hidden');
  const app = $('#app');
  app.classList.remove('with-panel');
  app.classList.remove('tools');
  app.classList.remove('stats');
}

async function showInspect() {
  if (!state.id) {
    note('open a session first');
    return;
  }
  const data = await get('/inspect').catch(() => null);
  if (!data) return;
  const rows = data.messages || [];
  const body = openPanel('inspect', `inspect · ${rows.length} messages`);

  const head = el('div');
  head.appendChild(
    el(
      'div',
      '',
      `${data.contextKnown === false ? 'no real measurement ' + (data.summary ? 'since the last compaction' : 'yet') : `${fmtN(data.contextUsed)} of ${fmtN(data.nCtx)} tokens`}${data.estimated ? ' (estimated: the provider gives no per-request usage)' : ''}`,
    ),
  );
  head.appendChild(
    el(
      'div',
      '',
      `budget ${fmtN(data.budget)} · summary cap ${fmtN(data.summaryCap)} · ${data.systemBytes} bytes of system prompt · ${data.totalBytes} bytes total${data.syntheticToolResults ? ` · ${data.syntheticToolResults} synthetic tool result(s)` : ''}`,
    ),
  );
  if (data.boundaryId) head.appendChild(el('div', '', `boundary ${data.boundaryId}${data.boundaryBroken ? ' (missing!)' : ''}`));
  // What the next compaction would do, planned by the server from the same entries it would send.
  // Shown when it succeeds as well as when it refuses: "what would a compaction here cost me" and "why
  // did nothing happen" are the same question, and this is the only place it can be asked first.
  if (data.plan) head.appendChild(el('div', '', planLine(data.plan, rows.length)));
  if (data.tools?.length) head.appendChild(el('div', '', `tools: ${data.tools.join(', ')}`));
  body.appendChild(head);

  // The system prompt is part of the payload, so it belongs in this view. Without it there is no way
  // to see what steers the model — including which language and script it should answer in. Older
  // servers report only systemBytes, so the block is skipped rather than lying about "(none)".
  if (typeof data.system === 'string') {
    body.appendChild(el('div', 'lbl', 'system prompt (first message, sent verbatim)'));
    body.appendChild(
      el('pre', '', data.system || 'none: no system message is sent, the model falls back to its own defaults (its trained language and script included)'),
    );
  }

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
