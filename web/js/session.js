// Session-level reads and renders: the transcript of the current session, the sidebar list, the
// status badges and the pending queue. This is the module that talks to the server about *sessions*;
// the transcript module only draws what it is given.
import { $, el, fmtN, fmtBytes, logEl, clamp, pathOf, sortByNum } from './util.js';
import { state } from './state.js';
import { api, get } from './api.js';
import { EV_SESSION, emit } from './bus.js';
import { confirmBox, note } from './ui.js';
import { isPinned, resetScroll, scrollDown } from './scroll.js';
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

// PAGE is what one read of the transcript asks for, and what every "older" press adds to it. Two
// numbers decided it: a whole path of this project's own test session serialises to 1.9 MB, and the
// cost of laying out a transcript grows with what is on it (a forced reflow measured 0.14 ms at 221
// characters of content and 52.88 ms once the same element had reached 214 000 pixels of height). A
// session is opened to read the newest part of it, so that is what the first request asks for.
const PAGE = 40;

// The chip that offers to read further back waits near the top of the transcript, because that is the
// only place where reading backwards happens. At the bottom the newest message is already there.
const REVEAL_PX = 200;

// pageQuery is the window this tab is showing, as a query string. The window always ends at the tip —
// reading backwards grows it at the older end — so the transcript never sees half a turn, and the
// parent walk in pathOf always starts from an entry that is present. Once the tab is holding more than
// a page may carry there is no page left to ask for, and asking for one would mean agreeing to forget
// the older entries already on screen: the whole path is asked for by name instead, which is never more
// than the unpaged read this endpoint made on every refresh before it could page. The cost panel is the
// other reader of this endpoint and it wants the whole path — it prices every request on the path — so
// `stats.js` keeps calling `get('')` unparameterised, and the default of a request with no query string
// staying "no page" is what makes that keep working.
function pageQuery() {
  const size = state.win ? state.win.size : PAGE;
  const max = state.win?.max || 0;
  return max && size > max ? '?all=1' : `?limit=${size}`;
}

// One page on its way at a time: a second press must not grow the window before the first one has
// landed, or the window grows twice for one response.
let loadingOlder = false;

// renderOlder writes the chip's own label: how much of the path is on screen, of how much, and what it
// cost to fetch. Every number is the server's, not the client's guess — including `count`, which is what
// came back rather than what was asked for: a tab that asked for 80 entries of a 66-entry path has 66 on
// screen, and printing the number it asked for would be a small lie about what is in front of the reader.
function renderOlder() {
  const chip = $('#older');
  if (!chip) return;
  const w = state.win;
  if (!w) {
    chip.classList.remove('shown');
    return;
  }
  chip.textContent = `↑ older · ${fmtN(w.count)} of ${fmtN(w.total)}`;
  chip.title =
    `the transcript is a page: ${fmtN(w.count)} of ${fmtN(w.total)} entries on the path are on screen, about ${fmtBytes(w.bytes)} to fetch.\n` +
    `click to read ${fmtN(PAGE)} more — the page grows at the older end and what you are looking at does not move.`;
  chip.disabled = loadingOlder;
  revealOlder();
}

// revealOlder shows the chip when there is something older AND the top of the transcript is on screen.
function revealOlder() {
  const chip = $('#older');
  if (!chip) return;
  chip.classList.toggle('shown', !!state.win?.hasOlder && logEl().scrollTop <= REVEAL_PX);
}

// loadOlder grows the page rather than fetching a separate older slice: one request, one render path,
// and the window stays contiguous with the tip. What it pays for that is re-fetching what is already on
// screen — which is what every refresh did unconditionally before the endpoint could page.
async function loadOlder() {
  if (loadingOlder || !state.win?.hasOlder) return;
  loadingOlder = true;
  state.win.size += PAGE;
  renderOlder();
  try {
    await refresh();
  } catch {
    state.win.size -= PAGE; // the page that did not arrive was never added to the window
  } finally {
    loadingOlder = false;
    renderOlder();
  }
}

// wireOlderRow is called once from app.js: the chip and the scroll position that reveals it.
export function wireOlderRow() {
  const chip = $('#older');
  if (!chip) return;
  chip.onclick = () => loadOlder();
  logEl().addEventListener('scroll', revealOlder, { passive: true });
}

// refresh re-reads the session and rebuilds the transcript. Streams that are still running survive:
// their nodes are re-appended below the committed entries, so a refresh in the middle of an answer
// does not blank the answer.
export async function refresh() {
  if (!state.id) return;
  const box = logEl();
  const asked = state.win ? state.win.size : PAGE;
  const data = await get(pageQuery());
  // Both config writers (settings dialog, tools drawer) PATCH a whole config built on top of
  // state.cfg, so a copy older than the last save silently undoes it — a price edited in the dialog
  // bounced back by the next tool toggle. This GET already carries the session's config, which makes
  // it the cheapest place to keep the cache a mirror. null on a missing block, so the drawer's `if
  // (!cfg)` refetch path still fires.
  state.cfg = data.config ?? null;
  // An empty transcript means "fresh load or session switch": that always lands on the newest
  // message, even if the viewport was left reading something older.
  const wasEmpty = !box.children.length;
  // A rebuild replaces every pixel of the transcript, so afterwards the viewport has to be pointed back
  // at the same place in the conversation. Distance from the bottom is the one number that survives a
  // change in height: the window only ever grows at the older end, so holding that distance holds the
  // entry the reader was looking at — the whole difference between “more arrived above” and “the page
  // jumped”. A fresh transcript and a reader following the tip have nothing to hold: for those the
  // bottom is the right place, and it is scrollDown() below that puts it there.
  const keep = wasEmpty || isPinned() ? null : box.scrollHeight - box.scrollTop;
  box.textContent = '';
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
  for (const [, s] of state.streams) box.appendChild(s.node);
  state.status = data.status;
  renderStatus(data.status);
  state.pending = data.pending || [];
  renderPending();
  const w = data.window || {};
  // `size` is what this tab asked for and keeps between reads; the rest is what the server says came
  // back. An answer with no `window` at all (an older server) leaves the chip out of the UI entirely,
  // because `hasOlder` can only ever be false — no promise is made about paging that cannot be kept.
  state.win = {
    size: asked,
    count: w.count || 0,
    oldest: w.oldestId || '',
    hasOlder: !!w.hasOlder,
    total: w.total || 0,
    bytes: w.bytes || 0,
    max: w.maxLimit || 0,
  };
  scrollDown(wasEmpty);
  // Reading backwards: the newest message stays exactly as far below the viewport as it was, which is
  // the whole difference between "more arrived above" and "the page jumped".
  if (keep !== null) box.scrollTop = box.scrollHeight - keep;
  // Last, because the chip decides whether to appear by asking where the top of the transcript is, and
  // the viewport has to be back where the reader left it before that question means anything.
  renderOlder();
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
  state.win = null; // and so does the page it asked for: a new session starts at its newest message
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
