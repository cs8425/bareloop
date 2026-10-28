// Session-level reads and renders: the transcript of the current session, the sidebar list, the
// status badges and the pending queue. This is the module that talks to the server about *sessions*;
// the transcript module only draws what it is given.
import { $, el, fmtN, logEl, clamp, pathOf, sortByNum } from './util.js';
import { state } from './state.js';
import { api, get } from './api.js';
import { confirmBox, note } from './ui.js';
import { resetScroll, scrollDown } from './scroll.js';
import { closeNav } from './nav.js';
import { entryNode } from './transcript.js';

// refresh re-reads the session and rebuilds the transcript. Streams that are still running survive:
// their nodes are re-appended below the committed entries, so a refresh in the middle of an answer
// does not blank the answer.
export async function refresh() {
  if (!state.id) return;
  const data = await get('');
  // An empty transcript means "fresh load or session switch": that always lands on the newest
  // message, even if the viewport was left reading something older.
  const wasEmpty = !logEl().children.length;
  logEl().textContent = '';
  state.entries.clear();
  state.tools.clear();
  const entries = sortByNum(data.entries || []);
  const onPath = pathOf(entries, data.status?.tip);
  for (const e of entries) {
    const node = entryNode(e);
    if (!onPath.has(e.id)) {
      // Another branch: stored, visible, but not part of what gets sent.
      node.style.opacity = 0.45;
      node.title = 'not on the current path (branch)';
    }
  }
  for (const [, s] of state.streams) logEl().appendChild(s.node);
  state.status = data.status;
  renderStatus(data.status);
  state.pending = data.pending || [];
  renderPending();
  scrollDown(wasEmpty);
}

export async function loadSessions() {
  const data = await api('/api/sessions').catch(() => null);
  if (!data) return;
  state.sessions = data.sessions || [];
  const list = $('#session-list');
  list.textContent = '';
  for (const s of state.sessions) {
    const li = el('li', s.id === state.id ? 'active' : '');
    li.appendChild(el('span', 'sid', s.id));
    li.appendChild(el('span', '', s.name || s.model || ''));
    li.appendChild(el('span', 'sid', `${s.phase || 'closed'} · ${s.entries} entries`));
    li.onclick = () => selectSession(s.id);
    const rm = el('button', 'ghost', '×');
    rm.title = 'delete this session and its log';
    rm.style.marginLeft = 'auto';
    rm.onclick = async (e) => {
      e.stopPropagation();
      const yes = await confirmBox(`delete session ${s.id}?`, {
        title: 'delete session',
        detail: 'Its directory under the data directory goes with it: config, history, every branch. This one is not reversible.',
        ok: 'delete',
        danger: true,
      });
      if (!yes) return;
      await api(`/api/session/${s.id}`, { method: 'DELETE' }).catch((err) => note(err.message, 'error'));
      if (state.id === s.id) {
        state.id = null;
        logEl().textContent = '';
      }
      loadSessions();
    };
    li.appendChild(rm);
    list.appendChild(li);
  }
}

// selectSession is the only place that clears the view: everything the previous session owned goes
// away at once, so no half of the page can render the wrong session.
export async function selectSession(id) {
  state.id = id;
  closeNav(); // on a phone the drawer covered the transcript only to pick this
  logEl().textContent = '';
  resetScroll();
  state.entries.clear();
  state.tools.clear();
  state.streams.clear();
  for (const [, card] of state.requests) card.remove();
  state.requests.clear();
  await loadSessions();
  await refresh();
}

export function renderStatus(st) {
  if (!st) return;
  const phase = $('#phase');
  phase.textContent = st.phase;
  phase.className = 'badge ' + st.phase;
  // The stop button replaces the compact button while a run is live: one control in that slot, and
  // stopping is the only thing worth reaching for mid-run.
  $('#btn-stop').classList.toggle('hidden', st.phase !== 'running');
  const servers = st.servers || [];
  const failed = servers.filter((s) => s.error || !s.connected);
  const tools = servers.reduce((n, s) => n + (s.tools || 0), 0);
  const toolsEl = $('#tools');
  toolsEl.textContent = `${st.tools ?? tools} tools${failed.length ? ` · ${failed.length} down` : ''}`;
  toolsEl.title =
    servers.map((s) => `${s.name}: ${s.connected ? s.protocolVersion || s.era || 'connected' : 'down'}${s.error ? ' — ' + s.error : ''} (${s.tools} tools)`).join('\n') ||
    'no MCP servers';
  $('#turn').textContent = st.turn ? `turn ${st.turn}` : '';
  const used = st.contextUsed || 0;
  const nctx = st.nCtx || 0;
  // contextKnown=false means the only number we have priced a payload that no longer exists —
  // right after a compaction, and for the one turn until a request is actually sent. Show a dash
  // rather than a stale percentage (and never a fake 0%).
  const known = st.contextKnown !== false;
  const pct = nctx > 0 && known ? clamp((100 * used) / nctx, 0, 100) : 0;
  $('#ctx-fill').style.width = pct.toFixed(1) + '%';
  $('#ctx-text').textContent =
    nctx > 0
      ? known
        ? `${fmtN(used)} / ${fmtN(nctx)} (${pct.toFixed(0)}%, trigger ${st.thresholdPct ?? '-'}%)${st.summary ? ' · summary' : ''}${st.boundaryBroken ? ' · boundary missing' : ''}`
        : `– / ${fmtN(nctx)} (no measurement${st.summary ? ' since the last compaction' : ' yet'})${st.summary ? ' · summary' : ''}${st.boundaryBroken ? ' · boundary missing' : ''}`
      : `${fmtN(used)} tokens · n_ctx unknown: set it in Settings (the server reported no window)`;
  $('#session-name').value = st.name || st.session || '';
}

// renderPending shows queued inputs: they are entries already in the log, so the list is a promise
// that the loop will get to them.
export function renderPending() {
  const box = $('#pending');
  const list = $('#pending-list');
  const badge = $('#pending-badge');
  list.textContent = '';
  if (!state.pending.length) {
    box.classList.add('hidden');
    badge.classList.add('hidden');
    return;
  }
  box.classList.remove('hidden');
  badge.classList.remove('hidden');
  badge.textContent = `pending: ${state.pending.length}`;
  for (const id of state.pending) list.appendChild(el('li', '', id));
}
