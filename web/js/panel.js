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

export function hidePanel() {
  $('#panel').classList.add('hidden');
  $('#app').classList.remove('with-panel');
}

async function showInspect() {
  if (!state.id) {
    note('open a session first');
    return;
  }
  const data = await get('/inspect').catch(() => null);
  if (!data) return;
  $('#panel').classList.remove('hidden');
  $('#app').classList.add('with-panel');
  const rows = data.messages || [];
  $('#panel-title').textContent = `inspect · ${rows.length} messages`;
  const body = $('#panel-body');
  body.textContent = '';

  const head = el('div');
  head.appendChild(
    el(
      'div',
      '',
      `${data.contextKnown === false ? 'no real measurement since the last compaction' : `${fmtN(data.contextUsed)} of ${fmtN(data.nCtx)} tokens`}${data.estimated ? ' (estimated: the provider gives no per-request usage)' : ''}`,
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
  if (data.planReason) head.appendChild(el('div', '', `next compaction plan: ${data.planReason} (cut ${data.plan?.cut ?? 0})`));
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
