// Session-level reads and renders: the transcript of the current session, the sidebar list, the
// status badges and the pending queue. This is the module that talks to the server about *sessions*;
// the transcript module only draws what it is given.
import { $, el, fmtN, fmtBytes, logEl, clamp, pathOf, sortByNum } from './util.js';
import { isParked, state } from './state.js';
import { api, get } from './api.js';
import { EV_SESSION, emit } from './bus.js';
import { confirmBox, note } from './ui.js';
import { isPinned, markPinned, resetScroll, scrollDown } from './scroll.js';
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

// pageQuery is the window this tab is showing, as a query string. There are two kinds of window, and
// which one is in effect is a field on `state.win` rather than a variable of its own, because the
// request and the facts about its answer have to survive a rebuild together:
//
//  - `tip`, the default: the window ends at the newest message, so reading backwards *grows* it at the
//    older end — nothing already on screen is dropped, and the parent walk in `pathOf` always starts
//    from an entry that is present. Once the tab is holding more than a page may carry there is no page
//    left to ask for, and asking for one would mean agreeing to forget the older entries already on
//    screen: the whole path is asked for by name instead, which is never more than the unpaged read this
//    endpoint made on every refresh before it could page. The cost panel is the other reader of this
//    endpoint and it wants the whole path — it prices every request on the path — so `stats.js` keeps
//    calling `get('')` unparameterised, and the default of a request with no query string staying "no
//    page" is what makes that keep working.
//  - parked: the window is a page *away from* the newest message, because the reader jumped to a turn.
//    `anchor` asks for the page centred on the turn holding `pivot`, `before` for the page that ends
//    just before it. Neither is ever combined with `all=1`: the endpoint takes `all` to mean "no page at
//    all" and answers `all` plus anything else with a 400.
function pageQuery() {
  const w = state.win;
  if (!w) return `?limit=${PAGE}`;
  if (w.mode === 'anchor') return `?anchor=${w.pivot}&limit=${w.size}`;
  if (w.mode === 'before') return `?before=${w.pivot}&limit=${w.size}`;
  return w.max && w.size > w.max ? '?all=1' : `?limit=${w.size}`;
}

// One page on its way at a time: a second press must not grow the window before the first one has
// landed, or the window grows twice for one response.
let loadingOlder = false;

// renderPaging keeps the two paging chips and the parked marker honest. It is called after every render
// and after every press, because what a chip may promise depends on where in the conversation the page
// is, and that is the server's answer rather than anything this module remembers.
function renderPaging() {
  const wrap = $('#log-wrap');
  // The corner #jump owns promises "the newest message", and while parked the bottom of this transcript
  // is not that. Toggling a class here instead of reaching into scroll.js leaves the button with the
  // module that draws it: scroll.js learns the transcript is parked from CSS, not from a call.
  if (wrap) wrap.classList.toggle('parked', isParked());
  renderOlder();
  renderNewer();
}

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
  const same = `about ${fmtBytes(w.bytes)} to fetch.`;
  chip.textContent = `↑ older · ${fmtN(w.count)} of ${fmtN(w.total)}`;
  // The label is the same in both window modes because the number means the same thing; what differs is
  // the promise, and a title that promised "+40 more" while parked would be promising a re-fetch.
  chip.title = isParked()
    ? `the transcript is parked on a page of it: ${fmtN(w.count)} of ${fmtN(w.total)} entries on the path are on screen, ${same}\nclick to read the page before this one — the window slides, so the newest entries you can see now are left behind and come back with "newer".`
    : `the transcript is a page: ${fmtN(w.count)} of ${fmtN(w.total)} entries on the path are on screen, ${same}\nclick to read ${fmtN(PAGE)} more — the page grows at the older end and what you are looking at does not move.`;
  chip.disabled = loadingOlder;
  revealOlder();
}

// renderNewer is the way back from a parked page, and nothing shows it when the transcript is not
// parked: at the tip there is nothing newer than the newest message, so a chip for it would be chrome on
// every screen that has nothing to go back from.
function renderNewer() {
  const chip = $('#newer');
  if (!chip) return;
  const w = state.win;
  const parked = isParked();
  chip.classList.toggle('shown', parked);
  if (!parked) {
    // Label and title go back to nothing rather than keeping the last parked page's numbers: a hidden
    // element holding "14 of 66" is a lie waiting for whoever shows it next.
    chip.textContent = '';
    chip.title = '';
    return;
  }
  chip.textContent = `${w.hasNewer ? '↓ newer' : '↓ latest'} · ${fmtN(w.count)} of ${fmtN(w.total)}`;
  chip.title =
    (w.hasNewer
      ? `this page is parked on ${w.pivot}: ${fmtN(w.count)} of ${fmtN(w.total)} entries on the path are on screen.\nclick to read forward — each click picks the page back up around the newest entry it holds, until it reaches the newest message.`
      : `this page already reaches the newest message; click to read the transcript from there again.`) +
    // The count of what was given up rather than the fact of it, because the transcript looks perfectly
    // calm while it is being ignored, and "how out of date am I" is the question a parked reader has.
    (w.stale ? `\n\n${fmtN(w.stale)} thing${w.stale === 1 ? '' : 's'} happened at the tip since this page was read, and none of it was drawn here.` : '');
  chip.disabled = loadingOlder;
}

// revealOlder shows the chip when there is something older AND the top of the transcript is on screen.
function revealOlder() {
  const chip = $('#older');
  if (!chip) return;
  chip.classList.toggle('shown', !!state.win?.hasOlder && logEl().scrollTop <= REVEAL_PX);
}

// loadOlder is one gesture with two moves behind it, and the window decides which one it is. At the tip
// the page *grows*: it ends at the newest message, so reading backwards means more of the same page, and
// re-fetching what is on screen is the price of staying on one request path. Parked, growing is not even
// available — `?limit=N` on a parked page asks for the page you are already standing on — so the window
// slides back instead, and lands at the bottom of what arrives: the end that continues what you were
// reading is the end closest to you.
async function loadOlder() {
  if (loadingOlder || !state.win?.hasOlder) return;
  const w = state.win;
  loadingOlder = true;
  const undo = { mode: w.mode, pivot: w.pivot, size: w.size };
  if (w.mode === 'tip') w.size += PAGE;
  else {
    w.mode = 'before';
    w.pivot = w.oldest;
    w.place = 'bottom';
  }
  renderPaging();
  try {
    await refresh();
  } catch {
    Object.assign(w, undo); // the page that did not arrive was never added to the window
    delete w.place; // and neither was the promise about where to land
  } finally {
    loadingOlder = false;
    renderPaging();
  }
}

// loadNewer walks a parked page forward. The endpoint has no `after=`, and inventing an id (newest + 1)
// would be a guess about the log's id space: ids are dense but an entry with the next number can live on
// another branch, and an anchor that is not on the path is answered with a 400. So forward is asked the
// way the server can always honour it — park on the newest entry this page already holds, which the
// server just sent and therefore exists on the path. That reaches the tip in a few presses instead of
// one, which is what an endpoint that pages by whole turns costs.
async function loadNewer() {
  if (loadingOlder || !isParked()) return;
  const w = state.win;
  if (!w.hasNewer) return returnToTip(); // this page already reaches the tip: "newer" means go home
  loadingOlder = true;
  const undo = { mode: w.mode, pivot: w.pivot };
  w.mode = 'anchor';
  w.pivot = w.newest;
  w.place = 'bottom';
  renderPaging();
  try {
    await refresh();
  } catch {
    Object.assign(w, undo);
    delete w.place;
  } finally {
    loadingOlder = false;
    renderPaging();
  }
}

// returnToTip drops the parked window. The page size is deliberately not carried over: it was grown to
// read backwards from a place, and back at the tip the older chip starts growing again from one page.
export function returnToTip() {
  state.win = null;
  // refresh() only puts the viewport at the bottom on its own while the transcript is pinned, and a
  // parked page is not: say so here, or the reader lands in the middle of the page they just left.
  markPinned();
  // ...and force it again once the read has landed. A reader who was parked has a viewport sitting in the
  // middle of the conversation, and holding that distance-from-bottom across a page swap stops the view
  // short of the newest message — measured 2 531 pixels short of it. "Back to the newest message" that
  // arrives 2 531 pixels away is half a promise.
  return refresh().then(() => scrollDown(true));
}

// jumpTo parks the transcript on a turn that is not near the tip. The tick ruler is what will call this
// once it exists, and any "who said this" link wants the same thing: the conversation from there, with
// the turn you asked for at the top of the viewport rather than somewhere in the middle of it.
export async function jumpTo(id) {
  if (!state.id || !id) return;
  // One page, always: a window grown to 200 entries to read backwards from the tip is the wrong shape
  // to land in the middle of a conversation with. `anchor` is the only way to ask for a page around an
  // entry, and the endpoint snaps it back to the turn start that owns the id, so this never lands on
  // half a turn.
  state.win = { mode: 'anchor', size: PAGE, pivot: id, stale: 0, place: id };
  await refresh();
}

// wirePaging is called once from app.js: the two chips, and the scroll position that reveals the top one.
export function wirePaging() {
  const older = $('#older');
  if (older) older.onclick = () => loadOlder();
  const newer = $('#newer');
  if (newer) newer.onclick = () => loadNewer();
  if (older) logEl().addEventListener('scroll', revealOlder, { passive: true });
}

// refresh re-reads the session and rebuilds the transcript. Streams that are still running survive:
// their nodes are re-appended below the committed entries, so a refresh in the middle of an answer
// does not blank the answer.
export async function refresh() {
  if (!state.id) return;
  const box = logEl();
  // The request currently on screen: `state.win` is only replaced at the bottom of this function, which
  // is what lets the window outlive the rebuild. `place` is read here and deliberately never written back
  // down — "put the viewport there" is a fact about the navigation landing now, not about the page, and
  // keeping it out of the request is what stops a jump from turning into a permanent instruction: an
  // entry arriving at the tip must not make every later event re-read the page someone parked on.
  const prev = state.win;
  const mode = prev?.mode || 'tip';
  const parked = mode !== 'tip';
  const size = prev?.size || PAGE;
  const pivot = prev?.pivot || '';
  const place = prev?.place || '';
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
  // change in height: while the window ends at the tip it only ever grows at the older end, so holding
  // that distance holds the entry the reader was looking at — the whole difference between “more arrived
  // above” and “the page jumped”. A fresh transcript and a reader following the tip have nothing to hold:
  // for those the bottom is the right place, and it is scrollDown() below that puts it there. A page that
  // was asked to land somewhere (place) has its own answer, and skipping this reads no layout at all.
  const keep = place || wasEmpty || isPinned() ? null : box.scrollHeight - box.scrollTop;
  box.textContent = '';
  state.entries.clear();
  state.tools.clear();
  state.toolResults.clear();
  const entries = sortByNum(data.entries || []);
  // The path is what gets sent, and it is the only thing the transcript shows. An entry that is not on it
  // belongs to another branch: with no tree view there is no way back to that branch, so drawing it would
  // put history on screen that leads nowhere and nobody can pick up again. It stays in the log.
  // The walk starts at the newest entry of the page rather than at the tip, and that is the only way it
  // can: a page parked away from the tip does not contain the tip, and a parent walk that cannot find its
  // starting point draws nothing at all. At the tip the two are the same entry, because a page that ends
  // at the tip has the tip as its newest entry — which is also why nobody noticed until a page did not.
  const onPath = pathOf(entries, data.window?.newestId || data.status?.tip);
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
  // The request goes back down unchanged and the facts come from the server. An answer with no `window`
  // at all (an older server) leaves both chips out of the UI, because `hasOlder` and `hasNewer` can only
  // ever be false — no promise is made about paging that cannot be kept.
  state.win = {
    mode,
    size,
    pivot,
    // What a parked page gave up while it was being read is no business of this response: it is the tally
    // of events the gate in events.js refused to draw, and it outlives a re-read of the same page because
    // a re-read of a parked page finds the same page with the tip further away than before.
    stale: parked ? prev?.stale || 0 : 0,
    count: w.count || 0,
    oldest: w.oldestId || '',
    newest: w.newestId || '',
    hasOlder: !!w.hasOlder,
    hasNewer: !!w.hasNewer,
    total: w.total || 0,
    bytes: w.bytes || 0,
    max: w.maxLimit || 0,
  };
  // Where the viewport goes is a separate question from what the rebuild contains. A parked page has no
  // bottom worth holding on to — the window slid, so both of its ends moved — so it is placed by what the
  // reader asked for: the entry they jumped to, or the bottom of the page that just arrived, which is the
  // end that continues the sentence they were reading.
  const target = place && place !== 'bottom' ? state.entries.get(place) : null;
  if (place === 'bottom') box.scrollTop = box.scrollHeight;
  else if (target) target.scrollIntoView({ block: 'start' });
  else {
    scrollDown(wasEmpty);
    // Reading backwards: the newest message stays exactly as far below the viewport as it was, which is
    // the whole difference between "more arrived above" and "the page jumped". This is also the fall-back
    // when a jump named an entry that did not arrive — a page is whole turns, so being handed a different
    // page than the one the id was on is something the endpoint is allowed to do.
    if (keep !== null) box.scrollTop = box.scrollHeight - keep;
  }
  // Last, because a chip decides whether to appear by asking where the top of the transcript is, and the
  // viewport has to be back where the reader left it before that question means anything.
  renderPaging();
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
    // Two counts, and they answer different questions: "path" is how many entries the conversation is, and
    // "log" how many records it took to write them. They part company as soon as anything is edited,
    // rewound, or compacted, because a patch and a delete are records that add no entry. The last figure is
    // what the log costs the disk, which is the only one of the three that never lies about a session whose
    // index is missing - and when it is missing, nothing is claimed about the other two: a zero would read
    // as an empty session.
    const stats = el('span', 'sid', s.bytes ? `${s.phase || 'closed'} · path ${fmtN(s.path)} · log ${fmtN(s.log)} · ~${fmtBytes(s.bytes)}` : `${s.phase || 'closed'} · –`);
    if (s.skipped) {
      // Ordinary traffic in a session an older build wrote: the index has a row for those records and this
      // build has no name for what they ask for. They are counted and left alone, which is not damage.
      stats.textContent += ` · ${fmtN(s.skipped)} ⓘ`;
      stats.title = `${fmtN(s.skipped)} record${s.skipped === 1 ? '' : 's'} the index has a row for and this build cannot act on: an op it has no name for, from a build older than this one. They stay in the log, and nothing about the session is broken by them.`;
    }
    li.appendChild(stats);
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
//
// PENDING_SHOWN caps how many queued ids are listed. Two numbers set it: the badge above already
// states the whole count, and a row of --t-1 measures 18 px, so eight rows plus the header stay
// inside the height the panel is allowed to take (24vh) without reaching for its own scrollbar. The
// head of the queue is the right end to show: pendingLocked walks the path root-to-tip, so the ids
// arrive oldest-first (measured on a 266-entry queue: 0000030, 0000031, …), and those are the
// entries the loop reads next.
// What is not listed is counted out loud, because a panel showing 8 of 266 and saying 8 would be a
// small lie about what is in front of the reader; 266 opaque ids were never readable anyway, and
// putting them all in the DOM is how the queue once took the transcript to 0 px of its own.
const PENDING_SHOWN = 8;

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
  for (const id of state.pending.slice(0, PENDING_SHOWN)) list.appendChild(el('li', '', id));
  if (state.pending.length > PENDING_SHOWN) {
    list.appendChild(el('li', 'more', `+ ${fmtN(state.pending.length - PENDING_SHOWN)} more`));
  }
}
