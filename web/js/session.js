// Session-level reads and renders: the transcript of the current session, the sidebar list, the
// status badges and the pending queue. This is the module that talks to the server about *sessions*;
// the transcript module only draws what it is given.
import { $, el, fmtN, logEl, clamp, pathOf, sortByNum } from './util.js';
import { state } from './state.js';
import { api, get } from './api.js';
import { EV_SESSION, emit } from './bus.js';
import { confirmBox, note } from './ui.js';
import { resetScroll, scrollDown } from './scroll.js';
import { closeNav } from './nav.js';
import { entryNode, linkToolResults } from './transcript.js';
import { syncComposer } from './composer.js';

// The URL names the session you are looking at: `#/s/<id>`. It is written here and read on boot, which is what
// makes a reload, a bookmark or a pasted link land on the same conversation instead of whatever the list
// happens to start with. Nothing else in the app touches the hash, so this module owns it - and that is also
// why it has to keep it honest: a hash naming a session that no longer exists gets corrected, not left there
// to fail on the next reload.
export const idFromHash = () =>
  location.hash.startsWith('#/s/') ? decodeURIComponent(location.hash.slice(4)) : '';

// writeHash reports whether it moved anything. Equal hashes are skipped, and that is also what keeps the
// hashchange listener from being called back on itself: selectSession writes the hash, the listener then
// sees the id it has already selected and stops. `replace` is for boot and corrections, where no history
// entry was asked for; picking a session is a real navigation and can be stepped back through.
export function writeHash(id, replace = false) {
  const url = id ? `#/s/${encodeURIComponent(id)}` : location.pathname + location.search;
  if ((id && location.hash === url) || (!id && !location.hash)) return false;
  history[replace ? 'replaceState' : 'pushState'](null, '', url);
  return true;
}

// refresh re-reads the session and rebuilds the transcript. Streams that are still running survive:
// their nodes are re-appended below the committed entries, so a refresh in the middle of an answer
// does not blank the answer.
export async function refresh() {
  if (!state.id) return;
  const data = await get('');
  // Both config writers (settings dialog, tools drawer) PATCH a whole config built on top of
  // state.cfg, so a copy older than the last save silently undoes it — a price edited in the dialog
  // bounced back by the next tool toggle. This GET already carries the session's config, which makes
  // it the cheapest place to keep the cache a mirror. null on a missing block, so the drawer's `if
  // (!cfg)` refetch path still fires.
  state.cfg = data.config ?? null;
  // An empty transcript means "fresh load or session switch": that always lands on the newest
  // message, even if the viewport was left reading something older.
  const wasEmpty = !logEl().children.length;
  logEl().textContent = '';
  state.entries.clear();
  state.tools.clear();
  state.toolResults.clear();
  const entries = sortByNum(data.entries || []);
  // The path is what gets sent, and it is the only thing the transcript shows. An entry that is not on it
  // belongs to another branch: with no tree view there is no way back to that branch, so drawing it would
  // put history on screen that leads nowhere and nobody can pick up again. It stays in the log.
  const onPath = pathOf(entries, data.status?.tip);
  state.pathIds = [...onPath].reverse(); // pathOf walks tip -> root; the restart confirm wants root -> tip
  for (const e of entries) {
    if (onPath.has(e.id)) entryNode(e);
  }
  // Every card this pass created is now old enough to be told how its call ended, because the tool entries
  // that answer them were drawn after the messages that asked.
  linkToolResults();
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
        state.cfg = null; // the cached config described the session that has just been deleted
        writeHash(''); // a conversation that has been deleted must not stay in the address bar
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
  writeHash(id); // the address bar follows the view, so a reload or a shared link comes back here
  state.id = id;
  state.cfg = null; // the cached config described the session we are leaving; refresh() below fills in this one's
  // Anything that had the previous session open (the tool drawer) is told before the new one renders,
  // so a panel can never end up showing one session underneath another one's name.
  emit(EV_SESSION, id);
  closeNav(); // on a phone the drawer covered the transcript only to pick this
  logEl().textContent = '';
  resetScroll();
  state.entries.clear();
  state.tools.clear();
  state.toolResults.clear();
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
  // The composer's right-hand slot belongs to the phase: send while idle, the red square and the bolt
  // while a run is live. One owner for that decision (composer.js), because the glyph and the Enter key
  // have to agree - Enter interrupts mid-run, so a send arrow sitting there would be a lie.
  syncComposer(st.phase === 'running');
  const servers = st.servers || [];
  const failed = servers.filter((s) => s.error || !s.connected);
  const tools = servers.reduce((n, s) => n + (s.tools || 0), 0);
  // The badge is the door to the tool catalogue, so it says three things: how many tools a request
  // would carry, how many the policy still wants once any run in flight ends (that is what ⏳ means),
  // and how many servers are down. `toolsEnabled`/`toolsTotal` are absent on an older server.
  const toolsEl = $('#tools');
  const hidden = st.toolsTotal && st.toolsEnabled !== undefined && st.toolsEnabled !== st.toolsTotal;
  toolsEl.textContent =
    `${st.tools ?? tools} tools` +
    (hidden ? ` · ${st.toolsEnabled}/${st.toolsTotal} on` : '') +
    (st.toolsPending ? ' ⏳' : '') +
    (failed.length ? ` · ${failed.length} down` : '');
  toolsEl.title =
    (servers.map((s) => `${s.name}: ${s.connected ? s.protocolVersion || s.era || 'connected' : s.disabled ? 'off (disabled)' : 'down'}${s.error ? ' — ' + s.error : ''} (${s.enabled ?? s.tools}/${s.tools} tools on)`).join('\n') ||
      'no MCP servers') +
    '\n\nclick for the tool catalogue: descriptions, parameters, cost, and the switches';
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
