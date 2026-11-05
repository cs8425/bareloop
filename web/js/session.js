// Session-level reads and renders: the transcript of the current session, the sidebar list, the
// status badges and the pending queue. This is the module that talks to the server about *sessions*;
// the transcript module only draws what it is given.
import { $, el, fmtN, fmtBytes, fmtAgo, logEl, clamp, pathOf, sortByNum } from './util.js';
import { isParked, state } from './state.js';
import { api, get } from './api.js';
import { EV_SESSION, emit } from './bus.js';
import { confirmBox, note, toast } from './ui.js';
import { atTheBottom, distanceFromBottom, isPinned, markPinned, resetScroll, scrollDown } from './scroll.js';
import { closeNav, navIsOpen } from './nav.js';
import { icon } from './icons.js';
import { entryNode, linkToolResults } from './transcript.js';
import { syncComposer } from './composer.js';
import { fmtFull, fmtTime } from './clock.js';

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

// SLIDE is how far one gesture moves the window: half a page. It is also the reason a slide re-fetches half of
// what it already had — the overlap is what makes the move invisible. A page is whole turns, so "half" is as
// exact as "forty" ever was; what is guaranteed is progress: the ids a slide dropped do not come back on the
// next one, which is exactly what asking the server for `anchor=<newest>` used to do (r72 §8.7②, 1.4 MB for
// one press).
const SLIDE = PAGE / 2;

// CEIL is how big a tip page may grow before it starts sliding instead. Below it, reading backwards costs
// nothing: the page keeps the newest message and grows at the older end, which is how this transcript has
// always behaved. At it, the window has an end, and "older than that" can only be paid for with the newest
// entries on screen.
const CEIL = 2 * PAGE;

// HYST_PX is the hysteresis, and it is the difference between a boundary that pages and a boundary that
// floods: once an edge has fired, the viewport has to be seen this far away from it before the edge may fire
// again. A page that lands with its top still under the trigger line is the case this exists for.
const HYST_PX = 2 * REVEAL_PX;

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
//  - parked: the window is a page *away from* the newest message, because the reader jumped to a turn or slid
//    the window off the end. `anchor` asks for the page centred on the turn holding `pivot`, `before` for the
//    page that ends just before it, `after` for the one that starts just after it. `after` is what a forward
//    slide asks for and the reason a forward slide no longer re-fetches the page it is leaving; it is
//    `before`'s mirror and just as exclusive. None of the three is ever combined with `all=1`: the endpoint
//    takes `all` to mean "no page at all" and answers `all` plus anything else with a 400.
function pageQuery() {
  const w = state.win;
  if (!w) return `?limit=${PAGE}`;
  if (w.mode === 'anchor') return `?anchor=${w.pivot}&limit=${w.size}`;
  if (w.mode === 'before') return `?before=${w.pivot}&limit=${w.size}`;
  if (w.mode === 'after') return `?after=${w.pivot}&limit=${w.size}`;
  return w.max && w.size > w.max ? '?all=1' : `?limit=${w.size}`;
}

// One page on its way at a time: a second press, or a second crossing of the same edge, must not move the
// window before the first one has landed — otherwise the window moves twice for one response. Every path that
// reads it asks before it acts and clears it in a `finally`.
let sliding = false;

// The hysteresis, as two flags: an edge may fire once, and it becomes eligible again only after a scroll has
// been seen with the viewport this far clear of it (`armFromPosition`). They start unarmed, which is what
// keeps a page shorter than the viewport — its top and its bottom in one screen — from paging itself the
// moment it arrives.
let armedTop = false;
let armedBottom = false;

// Paging that moved nothing. A page is whole turns, so there are boundaries where `before` the first turn of
// this page and `after` its last both answer with the page already in front of you. That is not a stuck
// window, it is the end of one — so it is said once, and an edge that cannot move stops being allowed to ask.
let stalled = 0;

// armFromPosition is the whole hysteresis rule, and it is the only reader of the layout that decides whether
// the next gesture pages. Called from the scroll handler and once per landed page — never from a timer, which
// is how a scroll listener stays a scroll listener.
function armFromPosition() {
  const box = logEl();
  armedTop = box.scrollTop > REVEAL_PX + HYST_PX;
  armedBottom = distanceFromBottom(box) > HYST_PX;
}

// pivotInward is the entry half a page in from the older end of what is on screen, and it is the one id both
// directions of a slide ask for: `before` it the page ends half a page further back, `after` it the page starts
// half a page further on. It is read off the page rather than computed from `oldest`/`newest` because the
// endpoint snaps every page to turn boundaries — an arithmetic guess at "twenty back" lands inside a turn the
// server will not split, and would ask for a page that is not the one the reader is leaving. A page no wider
// than a step asks from its own far edge, so the step costs nothing it still has.
function pivotInward(forward) {
  const ids = state.pathIds || [];
  if (!ids.length) return '';
  if (ids.length <= SLIDE) return forward ? ids[ids.length - 1] : ids[0];
  return ids[SLIDE];
}

// slide moves the window by half a page, and it is the same gesture read from either end: the older end gives
// up the newest half, the newer end gives up the oldest half. `place` is the whole difference — the end of the
// new page closest to the reader is the end they came from — and landing on an edge is also what makes a late
// picture harmless: an image that grows afterwards pushes the content the reader is *not* looking at.
async function slide(forward) {
  const w = state.win;
  const pivot = pivotInward(forward);
  if (!pivot || sliding) return;
  sliding = true;
  const was = { mode: w.mode, pivot: w.pivot, size: w.size };
  const saw = { oldest: w.oldest, newest: w.newest };
  w.mode = forward ? 'after' : 'before';
  w.pivot = pivot;
  w.size = PAGE; // the band keeps one page's width; what it grew to reading backwards is not a shape to carry
  w.place = forward ? 'top' : 'bottom';
  renderPaging();
  try {
    await refresh();
    const now = state.win;
    if (now && now.count && now.oldest === saw.oldest && now.newest === saw.newest) {
      stalled++;
      if (stalled === 1) note(forward ? 'that is as new as this transcript reaches' : 'that is as old as this transcript reaches');
    } else stalled = 0;
  } catch (err) {
    Object.assign(w, was); // the page that did not arrive was never added to the window
    delete w.place; // and neither was the promise about where to land
    note(`could not read ${forward ? 'newer' : 'older'} entries: ${(err && err.message) || 'the daemon said no'}`);
  } finally {
    sliding = false;
    renderPaging();
  }
}

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
  // Three promises and three shapes of window, printed with the server's numbers: at the tip there is room to
  // grow, at the ceiling there is not, and parked the only way back to what is below is the other chip.
  chip.title = isParked()
    ? `this transcript is a window onto ${fmtN(w.count)} of ${fmtN(w.total)} entries on the path — ${same}\nreaching the top slides it back ${fmtN(SLIDE)} entries: the newest ${fmtN(SLIDE)} go, and come back with "newer". Clicking does the same thing.`
    : w.size >= CEIL
      ? `the window holds ${fmtN(w.count)} of ${fmtN(w.total)} entries on the path and does not grow past ${fmtN(CEIL)} — ${same}\nreaching the top slides it back ${fmtN(SLIDE)} entries, giving up the newest ones; they come back with "newer".`
      : `the transcript is a page: ${fmtN(w.count)} of ${fmtN(w.total)} entries on the path are on screen, ${same}\nreach the top or click: the page grows ${fmtN(PAGE)} entries at the older end and what you are looking at does not move.`;
  chip.disabled = sliding;
  revealOlder();
}

// renderNewer is the way back from a parked page, and nothing shows it when the transcript is not
// parked: at the tip there is nothing newer than the newest message, so a chip for it would be chrome on
// every screen that has nothing to go back from.
//
// It is exported for one reason: the parked gate in events.js counts what it refused to draw, and this is
// the only place those words are written. Nothing else rewrites the chip while parked — a parked page is
// precisely the one that does not refresh — so without this call the count is computed and never shown.
// Calling it is cheap: two text nodes, one attribute, and no layout read.
export function renderNewer() {
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
      ? `this window sits on ${fmtN(w.count)} of ${fmtN(w.total)} entries on the path, ${w.pivot} and older in hand.\nreaching the bottom slides it forward ${fmtN(SLIDE)} entries: the oldest ${fmtN(SLIDE)} go, the newest ${fmtN(SLIDE)} come in. Clicking does the same thing.`
      : `this page already reaches the newest message; click to read the transcript from there again.`) +
    // The count of what was given up rather than the fact of it, because the transcript looks perfectly
    // calm while it is being ignored, and "how out of date am I" is the question a parked reader has.
    (w.stale ? `\n\n${fmtN(w.stale)} thing${w.stale === 1 ? '' : 's'} happened at the tip since this page was read, and none of it was drawn here.` : '');
  chip.disabled = sliding;
}

// revealOlder shows the chip when there is something older AND the top of the transcript is on screen.
function revealOlder() {
  const chip = $('#older');
  if (!chip) return;
  chip.classList.toggle('shown', !!state.win?.hasOlder && logEl().scrollTop <= REVEAL_PX);
}

// loadOlder is the ↑ chip, and which of two things "older" means is the window's decision. At the tip and
// under the ceiling the page *grows*: it still ends at the newest message, nothing on screen is given up, and
// re-fetching what is already drawn is the price of staying on one request path. At or over the ceiling,
// growing is not even available — a window with a ceiling cannot also hold the end of the conversation — so it
// slides back instead, gives up the newest half, and lands at the bottom of what arrives: the end that
// continues what the reader was reading is the end closest to them. That is the moment the transcript stops
// following the tip, which is why `#newer` has something to say from here on.
async function loadOlder() {
  if (sliding || !state.win?.hasOlder) return;
  const w = state.win;
  if (w.mode !== 'tip' || w.size >= CEIL) return slide(false);
  sliding = true;
  w.size += PAGE;
  renderPaging();
  try {
    await refresh();
  } catch (err) {
    w.size -= PAGE; // the page that did not arrive was never added to the window
    note(`could not read older entries: ${(err && err.message) || 'the daemon said no'}`);
  } finally {
    sliding = false;
    renderPaging();
  }
}

// loadNewer is the ↓ chip. On a page that already reaches the tip, "newer" has one meaning left: go home.
// `returnToTip` re-reads the tip page, which is also how a page that caught up with a tip that kept moving
// gets back in step with it.
async function loadNewer() {
  if (sliding || !isParked()) return;
  if (!state.win.hasNewer) return returnToTip();
  return slide(true);
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
  const was = state.win; // the page on screen, in case the one asked for is refused
  state.win = { mode: 'anchor', size: PAGE, pivot: id, stale: 0, place: id };
  try {
    await refresh();
  } catch (err) {
    // A jump the endpoint refuses — an id off the path, deleted, on another branch: the answer is a 400 — has to
    // leave the page the reader is actually looking at. `state.win` is both the request and the truth about the
    // screen, so a window naming a page nobody drew hides "latest" behind the parked rule and fills both chips
    // with numbers for a transcript that is not on it. The caller says what failed.
    state.win = was;
    throw err;
  }
}

// onTranscriptScroll is the one place the transcript's own scroll position becomes a decision, and it makes
// three of them on one read of the layout: whether the ↑ chip has earned its place, whether the reader has
// crossed an edge of the window, and — parked on a page that already reaches the tip, with news this page
// refused to draw — whether being dragged to the bottom is the reader asking to go home.
//
// Crossing an edge is the change from how this worked: the chips used to be the only way to page, and a chip is
// chrome that has to be noticed first. An edge is not noticed, it is crossed, so the gesture that already means
// "further" now pages — once, and not again until the reader has come back `HYST_PX` away from that edge.
// It pages in both directions, which overrides the older rule that a mid-conversation page should be left
// alone when a thumb slips to the bottom of the screen: the window is a band now, and the bottom of a band is
// where "newer" is asked (D96 甲 in _state/). `stalled` is the same rule from the other side — an edge that
// answers with the page already on screen is an end, and an end stops asking.
function onTranscriptScroll() {
  revealOlder();
  const w = state.win;
  if (sliding || !w) return; // a page already on its way answers the same question
  const box = logEl();
  const atTop = box.scrollTop <= REVEAL_PX;
  // Read the flags before they are recomputed. An edge that fires is an edge that was armed *before* this
  // scroll, and this scroll is what spends it; recomputing first is a rule that erases its own evidence —
  // arriving at the top is itself the position that sets the top flag to false, so the page would never come.
  // Spending needs no line of its own: a firing edge is by definition inside its own hysteresis band, which is
  // exactly what `armFromPosition` disarms.
  const wasArmedTop = armedTop;
  const wasArmedBottom = armedBottom;
  armFromPosition();
  if (atTop) {
    if (!wasArmedTop || !w.hasOlder || stalled > 2) return;
    loadOlder().catch(() => {});
    return;
  }
  // At the tip there is nothing newer than the newest message, and keeping the tail on screen is the follow in
  // scroll.js, not this function's business.
  if (!isParked() || !atTheBottom() || !wasArmedBottom) return;
  if (w.hasNewer) {
    if (stalled <= 2) loadNewer().catch(() => {});
    return;
  }
  if (w.stale) returnToTip().catch(() => {});
}

// wirePaging is called once from app.js. The two chips stay as the deliberate, click-on-it form of the same two
// moves — a gesture that pages without asking is a good default and a bad monopoly — and one passive scroll
// listener answers every question the position can ask. No guard on the chips existing: each half already
// answers "no chip, nothing to say".
export function wirePaging() {
  const older = $('#older');
  if (older) older.onclick = () => loadOlder();
  const newer = $('#newer');
  if (newer) newer.onclick = () => loadNewer();
  logEl().addEventListener('scroll', onTranscriptScroll, { passive: true });
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
  // A parked page that came back with nothing in it is not an empty conversation. The pivot moved under us —
  // the tip ran on while this page sat there, or something deleted the entries the page was cut from — and
  // drawing an empty answer would take the page away from a reader who asked for one further along. This is an
  // error, and the caller still has the window it asked for: nothing above this line has touched the DOM.
  if (parked && !(data.entries || []).length) throw new Error('that page is no longer there');
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
  // A stream still in flight survives the rebuild: its node goes back below the committed entries, so a
  // refresh in the middle of an answer does not blank the answer. Two conditions say a stream does not come
  // back, and each one is a blank bubble somebody reported:
  //   - it has finished (`s.done`, set by transcript.js streamFinish): its reply is in the entries above by now.
  //   - it has drawn nothing yet (no text, no tool card): there is no answer in it to keep, and a header with
  //     no body under it is exactly the "an empty assistant appeared after the summary / after reconnecting"
  //     report. Both reports are the same rebuild — a compaction asks for one, a reconnect asks for one, and a
  //     socket that died never gets the event that would have retired the buffer — and a node with nothing in
  //     it is not content, it is chrome. A stream that is genuinely mid-answer keeps every character, because
  //     `content` is what it is judged on, and the next delta puts the node back if it was left out (see
  //     streamDelta).
  for (const [, s] of state.streams) if (!s.done && (s.content || s.tools.size)) box.appendChild(s.node);
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
  const target = place && place !== 'bottom' && place !== 'top' ? state.entries.get(place) : null;
  if (place === 'bottom') box.scrollTop = box.scrollHeight;
  // A slide forward lands on its top edge: on a page that moved towards the tip, the end closest to the reader
  // is the oldest entry that arrived, and holding an edge is also what makes a picture that grows later push
  // the content the reader is not looking at.
  else if (place === 'top') box.scrollTop = 0;
  else if (target) target.scrollIntoView({ block: 'start' });
  else {
    scrollDown(wasEmpty);
    // Reading backwards: the newest message stays exactly as far below the viewport as it was, which is
    // the whole difference between "more arrived above" and "the page jumped". This is also the fall-back
    // when a jump named an entry that did not arrive — a page is whole turns, so being handed a different
    // page than the one the id was on is something the endpoint is allowed to do.
    if (keep !== null) box.scrollTop = box.scrollHeight - keep;
  }
  // A page of one's own is a fresh start for the hysteresis too: back at the tip there is no reason to be
  // holding a grudge against an edge that stopped moving two pages ago.
  if (!parked) stalled = 0;
  // Last, because a chip decides whether to appear by asking where the top of the transcript is, and the
  // viewport has to be back where the reader left it before that question means anything.
  renderPaging();
  // ...and the edges get their vote on the same position the chips just read: armed only where the viewport is
  // not sitting on them, so a page shorter than the screen arrives without paging itself into the next one.
  armFromPosition();
}

// --------------------------------------------------------------------------- the list of sessions
//
// The list is a view of GET /api/sessions, and it adds exactly one thing to what the server said: where a
// row is drawn. Four groups, in this order, and a row belongs to one of them -
//
//   Pinned     somebody asked to keep it in reach
//   Running    its phase is running or compacting
//   Sessions   everything else
//   Archived   somebody asked to stop seeing it, and it starts out folded away
//
// `groupOf` is the one place that decides, and it asks what the reader asked before it asks what the session
// is doing: archiving a running session has to move it out of the way or it archived nothing. The two asks
// can never both be true of one session, and the daemon is what makes that so (see agent.applyFlags) - the
// cost is that pinning a session and then archiving it spends the pin, which is visible in the row that
// moved, and it is paid once instead of leaving four rules reaching for the same row.
//
// Inside a group the newest `lastTs` is on top, a session with no time to read sits at the bottom, and the
// id breaks every tie. Not for tidiness: an order that moves between two listings of an unchanged directory
// is a list that jumps sideways under a cursor reaching for the third row.
//
// One caveat is a server fact rather than a rendering one, and it is worth knowing before trusting a dot:
// only the session this tab is subscribed to pushes its phase, so the dots and the Running group describe
// each session as of the last time this list was read. A live view of sessions nobody is watching wants an
// event stream of its own.

// GROUPS is the order and the words. Nothing else about a group is data: which rows are in it is `groupOf`,
// and whether it is drawn is decided by `listPlan`.
const GROUPS = [
  { key: 'pinned', title: 'Pinned' },
  { key: 'running', title: 'Running' },
  { key: 'other', title: 'Sessions' },
  { key: 'archived', title: 'Archived' },
];

function groupOf(s) {
  // why archived first: it is the only one of the four the reader asked for, and an archived session with a
  // live phase showing up under "Running" would be the fold failing to fold.
  if (s.archived) return 'archived';
  // and pinned before the phase, because a pinned session that started running is the same session the
  // reader put at the top of the list on purpose.
  if (s.pinned) return 'pinned';
  if (s.phase === 'running' || s.phase === 'compacting') return 'running';
  return 'other';
}

// The Archived fold. It is a preference of this window and so it lives in memory rather than in storage -
// `bareloop.navCollapsed` remembers a viewport and this remembers one group of one list, and neither is a
// fact about a session. `forced` is what the two rules below auto-opened it for, so a reader who folds the
// group away again is not overridden by the next event one second later: the rule fires once per reason.
let archivedOpen = false;
let archivedForced = '';

// hitOne is the filter, and the one question it answers is whether a reader who typed this word would want
// this session in the answer. Id, name and model are the three figures somebody reading a list of sessions
// actually knows; the transcript is not searched, which is a different tool with a server behind it.
const hitOne = (s, filter) => !filter || `${s.id} ${s.name || ''} ${s.model || ''}`.toLowerCase().includes(filter);

// listPlan is the plan for <ul>'s children: a flat array, in the order they will be appended, of `{grp}`
// labels and `{row}` sessions. Flat because the diff below is positional: the DOM has children, not groups.
function listPlan(sessions, filter) {
  const newest = (a, b) => {
    // 0 for "nobody claimed a time", which sorts below every timestamp and stays at the bottom of the group
    // rather than in the company of last month.
    const at = a.lastTs ? Date.parse(a.lastTs) : 0;
    const bt = b.lastTs ? Date.parse(b.lastTs) : 0;
    return bt - at || (a.id < b.id ? -1 : a.id > b.id ? 1 : 0);
  };
  const want = [];
  for (const g of GROUPS) {
    const all = sessions.filter((s) => groupOf(s) === g.key);
    if (!all.length) continue; // an empty group costs the list nothing, including a label
    const rows = all.filter((s) => hitOne(s, filter)).sort(newest);
    const collapsed = g.key === 'archived' && !archivedOpen;
    // The count says one of three things and never more than one: ` (3)` hidden away, ` (3)` all of them
    // drawn, ` (1/3)` the filter is hiding two. Reading the group label is how you find out the filter did
    // not simply find nothing.
    const count = collapsed || rows.length === all.length ? `${all.length}` : `${rows.length}/${all.length}`;
    // `hint` and not `note`: ui.js exports a function called note, and shadowing an import inside the one
    // helper that may later want to complain out loud is a bug with a five-year fuse.
    let hint;
    if (collapsed) {
      hint = `${all.length} archived ${all.length === 1 ? 'session' : 'sessions'}, hidden - click to show them`;
    } else if (rows.length < all.length) {
      hint = `${all.length - rows.length} of ${all.length} do not say “${filter}” anywhere`;
      if (g.key === 'archived') hint += ' - click to fold them away again';
    } else if (g.key === 'archived') {
      hint = 'click to fold them away again';
    }
    want.push({ kind: 'grp', key: g.key, name: g.title, count, title: `${g.title} (${count})`, hint, collapsed });
    if (!collapsed) for (const s of rows) want.push({ kind: 'row', s });
  }
  // A list with no rows says why, because the three reasons look identical from here and are not the same
  // news: nothing matches the word in the box, everything is folded away, or there is no session yet.
  if (!want.some((w) => w.kind === 'row')) {
    const folded = want.some((w) => w.kind === 'grp' && w.collapsed);
    want.push({
      kind: 'none',
      title: filter
        ? `no session says “${filter}”`
        : folded
          ? 'every session is archived - click Archived above'
          : 'no session yet - the + above makes one',
    });
  }
  return want;
}

// sameRow is whether two copies of one session would be drawn the same. Every figure the row is built out of
// is in here and nothing that is not: the list is fetched afresh on every call, so the objects are never the
// same object twice, while what can be the same is the eleven things a row says. Add a figure to a row and
// this comparison has to be told, or that figure quietly stops updating.
function sameRow(a, b) {
  return (
    !!a && !!b &&
    a.id === b.id &&
    a.name === b.name &&
    a.model === b.model &&
    a.phase === b.phase &&
    !!a.closed === !!b.closed &&
    a.path === b.path &&
    a.log === b.log &&
    a.bytes === b.bytes &&
    a.skipped === b.skipped &&
    a.tip === b.tip &&
    !!a.pinned === !!b.pinned &&
    !!a.archived === !!b.archived &&
    a.lastTs === b.lastTs
  );
}

// rowTitle is the whole truth about a session in the space of a tooltip: the row itself only has room for a
// name, an age and three numbers, and everything the row shortened is here in full.
function rowTitle(s) {
  const line = [];
  line.push(s.id);
  if (s.model) line.push(s.model);
  // Two counts that answer different questions: "path" is how many entries the conversation is, "log" how
  // many records it took to write them. They part company as soon as anything is edited, rewound or
  // compacted, because a patch and a delete are records that add no entry.
  line.push(s.bytes ? `path ${fmtN(s.path)} entries · log ${fmtN(s.log)} records` : 'no index yet: nothing is claimed about its size');
  if (s.bytes) line.push(`log.jsonl is ~${fmtBytes(s.bytes)}`);
  if (s.tip) line.push(`standing on entry ${s.tip}`);
  if (s.closed) line.push('shut: opening it reads its log again');
  // The one figure the row prints in shorthand, printed in full. It is the newest entry's own timestamp and
  // not the log's mtime: a rewind leaves the entries as old as they were, and this says so.
  line.push(s.lastTs ? `newest entry ${fmtFull(s.lastTs)}` : 'newest entry: this log has no timestamp to read');
  if (s.pinned) line.push('pinned');
  if (s.archived) line.push('archived');
  if (s.skipped) {
    // Ordinary traffic in a session an older build wrote: the index has a row for those records and this
    // build has no name for what they ask for. They are counted and left alone, which is not damage.
    line.push(`${fmtN(s.skipped)} record${s.skipped === 1 ? '' : 's'} the index has a row for and this build cannot act on - an op it has no name for, from a build older than this one. They stay in the log, and nothing about the session is broken by them.`);
  }
  return line.join('\n');
}

// rowNode draws one session. Two lines, neither of which is allowed to wrap: the row used to be three spans
// in a 210px flex row, and an id like `00-mellow-maple-s4g` answered by splitting itself over two lines -
// thirteen rows came to 984px of list, 50 to 102px apiece. The fix is in app.css (`min-width:0`, `nowrap`,
// an ellipsis); what is drawn here is what that leaves room for.
function rowNode(s) {
  const li = el('li', `${s.id === state.id ? 'active ' : ''}${s.archived ? 'dim' : ''}`.trim());
  li.title = rowTitle(s);
  li.tabIndex = 0; // the row is the way to a session, so it is in the tab order; the buttons in it are stops of their own
  li.onclick = () => selectSession(s.id);
  li.onkeydown = (e) => {
    if (e.key !== 'Enter' && e.key !== ' ') return;
    e.preventDefault(); // space is a page scroll otherwise, and this row is not the page
    selectSession(s.id);
  };

  const top = el('div', 'row');
  const dot = el('span', `dot ${s.closed ? 'closed' : s.phase || 'off'}`);
  dot.setAttribute('aria-hidden', 'true');
  dot.title = s.closed ? 'shut' : s.phase || 'idle';
  top.appendChild(dot);
  if (s.pinned) {
    // A pinned row has to stay a pinned row when the actions move off it (that is what a hover reveal does
    // to a mark), so the pin is drawn rather than being one of the buttons' states.
    const mark = icon('pin', 'pinmark ico');
    mark.setAttribute('title', 'pinned');
    top.appendChild(mark);
  }
  // The name, or the id when there is no name: an id is a directory name and a poor thing to scan a list of,
  // which is why `name` exists at all.
  const nm = el('span', 'col nm', s.name || s.id);
  if (s.name) nm.title = `${s.name}\n${s.id}`;
  top.appendChild(nm);
  const ago = el('span', 'ago', fmtAgo(s.lastTs));
  ago.textContent = ago.textContent || '–'; // no time to read is a dash, and never a guess at one
  top.appendChild(ago);
  li.appendChild(top);

  const sub = el('div', 'sub col', s.bytes ? `${fmtN(s.path)} path · ${fmtN(s.log)} log · ${fmtBytes(s.bytes)}` : `${s.closed ? 'shut' : s.phase || 'idle'} · –`);
  // The skipped count is spelled with letters and not a symbol because this line is set in the mono stack
  // and a symbol outside it inherits a fallback face with a taller line box (see the `line-height` on
  // `.sub`). What the marker means is written out in full in the row's title.
  if (s.skipped) sub.textContent += ` · ${fmtN(s.skipped)} old`;
  li.appendChild(sub);

  li.appendChild(actions(s));
  return li;
}

// actions are the three things a reader does to a session rather than with it. They are absolutely
// positioned over the tail of the row and appear on hover, because the alternative is a row of three buttons
// permanently stealing the 80px that a session's name is written with - and on glass, where nothing hovers,
// app.css puts them back in the flow and takes the age away instead.
function actions(s) {
  const box = el('span', 'acts');
  const btn = (name, glyph, on) => {
    const b = el('button', `act ${name}`);
    b.type = 'button';
    b.appendChild(icon(glyph, 'ico'));
    b.title = b.ariaLabel = typeof on === 'string' ? on : '';
    return b;
  };
  const pin = btn('pin', 'pin', s.pinned ? 'unpin this session' : 'keep this session above the rest');
  pin.setAttribute('aria-pressed', s.pinned ? 'true' : 'false');
  pin.onclick = (e) => {
    e.stopPropagation();
    setFlag(s, { pinned: !s.pinned });
  };
  // One button, and its word is the row's state, because "archive" and "unarchive" are one question and it
  // is rude to answer it with an icon a reader has to hover to disambiguate.
  const arc = btn('arc', s.archived ? 'unarchive' : 'archive', s.archived ? 'put this session back in the list' : 'take this session out of the list');
  arc.onclick = (e) => {
    e.stopPropagation();
    setFlag(s, { archived: !s.archived });
  };
  const del = btn('del', 'trash', 'delete this session and its log');
  del.onclick = async (e) => {
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
  // Every open row can answer for itself: what this releases is the daemon's copy of that session, which is
  // worth having back most for the conversations you are not looking at. A closed row gets no button — the list
  // snapshot already knows that (unlike which row this window is showing), and there is nothing there to do.
  let down = null;
  if (!s.closed) {
    down = btn('down', 'close', 'let the daemon put this session down — the next read re-opens it from disk');
    down.onclick = (e) => {
      e.stopPropagation();
      unloadSession(s.id);
    };
  }
  box.append(pin, arc);
  if (down) box.append(down);
  box.append(del);
  return box;
}

// setFlag is the POST, the note when it fails, and the reload that shows what just moved. The answer is
// refused rather than believed: the group a row belongs to is decided here from the flags the list gave,
// and redrawing one row from a body this module has not read is how a list starts telling a story the
// daemon never agreed to. One extra GET, once per click.
async function setFlag(s, flags) {
  try {
    await api(`/api/session/${encodeURIComponent(s.id)}/flags`, { body: flags });
  } catch (err) {
    note(err.message, 'error');
    return;
  }
  loadSessions();
}

// loadSoon is the coalesced path, for callers that are an event rather than a click. One running session
// pushes a `state` event per tick, and each of them used to be a GET /api/sessions followed by every row in
// the sidebar rebuilt - a list that is a snapshot, not a stream, so the requests after the first were
// duplicates rather than news.
//
// Trailing-edge only, and that is the choice rather than an oversight: answering the first event of a burst
// immediately is what makes a burst cost two requests instead of one, and a quarter of a second is not a
// delay anybody attributes to a number in the sidebar. `loadSessions` stays the immediate call for the cases
// where a reader is already looking at the answer (a click on a row, a session created or deleted).
let soonTimer = null;
export function loadSoon() {
  if (soonTimer) return;
  soonTimer = setTimeout(() => {
    soonTimer = null;
    loadSessions();
  }, 250);
}

// What the last call drew, so the next one can tell whether it has any work to do.
let lastWant = [];

export async function loadSessions(opts = {}) {
  const data = await api('/api/sessions').catch(() => null);
  if (!data) return;
  state.sessions = data.sessions || [];
  const raw = ($('#session-filter')?.value || '').trim();
  const filter = raw.toLowerCase();
  // Two reasons the fold opens itself, and neither is a preference: the conversation on screen is inside it
  // (a transcript with no row of its own in the list is a session nobody can find again), and a filter that
  // only the hidden rows can answer (a fold hiding the only match is a search that returns nothing on
  // purpose). Once per reason is the whole point - the rule is "do not trap the reader", not "a closed fold
  // is a fact this module overrides every second" - so folding it away by hand sticks until the reason
  // changes: another session on screen, or another word in the box.
  const active = state.sessions.find((s) => s.id === state.id);
  const hides = state.sessions.some((s) => s.archived && hitOne(s, filter));
  const reason = active?.archived ? `active:${active.id}` : filter && hides ? `filter:${filter}` : '';
  if (reason && reason !== archivedForced) {
    archivedOpen = true;
    archivedForced = reason;
  }
  const want = listPlan(state.sessions, filter);
  const list = $('#session-list');
  // Nothing moved, so nothing is touched. `state` events arrive for every running session's tick and used to
  // be answered by rebuilding all thirteen rows and throwing the scroll position away with them.
  const dirty = want.map((w, i) => {
    const o = lastWant[i];
    if (!o || o.kind !== w.kind) return true;
    if (w.kind === 'grp') return o.title !== w.title || o.collapsed !== w.collapsed;
    // The "why is this list empty" row carries its own sentence, and that is the only thing it can change.
    // Left out, it would count as dirty on every call and be rebuilt once a second forever.
    if (w.kind === 'none') return o.title !== w.title;
    return !sameRow(o.s, w.s);
  });
  if (want.length !== lastWant.length || dirty.some(Boolean)) {
    const at = list.scrollTop;
    if (want.length !== lastWant.length) {
      list.textContent = '';
      want.forEach((w) => list.appendChild(nodeOf(w)));
    } else {
      dirty.forEach((d, i) => {
        if (!d) return;
        const next = nodeOf(want[i]);
        const now = list.children[i];
        // replaceChild and not `if (dirty) rebuild`: the whole row is a handful of nodes built from the same
        // fields in the same order, so replacing one of them is cheaper than re-deriving which text node of
        // it changed - and it keeps the row's listeners attached to the row that owns them.
        if (now) list.replaceChild(next, now);
        else list.appendChild(next);
      });
    }
    list.scrollTop = at; // an unchanged height lands where it was; a shorter list is clamped by the browser
    lastWant = want;
  }
  // The highlight is reconciled here and not left to the diff above, because `sameRow` compares what the
  // *server* says about a session and `state.id` is no such thing: choosing a session changes not one figure
  // of the plan, so the diff reports "nothing moved" and the class stays wherever it was. Measured before the
  // fix: a reload of `#/s/test-1` drew thirteen rows and highlighted none of them, and clicking another row
  // moved the transcript while the highlight stayed behind. `toggle(name, force)` is a no-op when the class is
  // already right, so a list reconciled once a second costs thirteen attribute reads and no mutation.
  syncActive(list, want);
  // The active row has to be on screen after a click that chose it, and only then: the same call runs once a
  // second under a running session, and a list that scrolls itself to whatever the server thinks is active
  // is a list a reader cannot read.
  if (opts.reveal) {
    const i = want.findIndex((w) => w.kind === 'row' && w.s.id === state.id);
    const row = i >= 0 ? list.children[i] : null;
    // block:'nearest' costs nothing when the row is already in sight, and there is no point in scrolling a
    // list nobody can see: on a collapsed desktop window the sidebar is a closed panel.
    if (row && !(document.body.classList.contains('nav-collapsed') && !navIsOpen())) row.scrollIntoView({ block: 'nearest' });
  }
}

// syncActive puts `active` on the row of the session on screen and on no other row. It is a loop and not a
// diff rule because the two figures it compares - the plan and `state.id` - are never both in the plan: the
// list is a snapshot of the daemon, and which one of the snapshots you are looking at is this window's news.
function syncActive(list, want) {
  for (let i = 0; i < want.length; i += 1) {
    const node = list.children[i];
    // why the kind is asked and not assumed: a group label and the "why is this list empty" row can never be
    // active, and skipping them is also what keeps this from clearing a class it did not put there.
    if (!node || want[i].kind !== 'row') continue;
    node.classList.toggle('active', want[i].s.id === state.id);
  }
}

// nodeOf is the one place a plan entry becomes a node, so a rebuild and a patch cannot draw two different
// versions of the same thing.
function nodeOf(w) {
  if (w.kind === 'row') return rowNode(w.s);
  if (w.kind === 'none') return el('li', 'none', w.title);
  const li = el('li', 'grp');
  // data-open is a CSS hook and aria-expanded is the accessible answer to the same question, and they are
  // two attributes because `aria-open` is not a thing a screen reader knows and a heading is not a thing a
  // screen reader expects to expand.
  li.dataset.open = w.collapsed ? 'false' : 'true';
  const chevron = icon('chevron', 'ico');
  chevron.setAttribute('aria-hidden', 'true');
  if (w.key !== 'archived') {
    // A heading and nothing more: Pinned, Running and Sessions do not fold, so this says what it is without
    // pretending to be a control. The chevron is appended anyway - the four labels line up because of it, and
    // app.css pays for that alignment with `visibility: hidden` rather than with another branch.
    const label = el('div', 'glabel');
    label.append(chevron, el('span', '', w.name), el('span', 'cnt', `(${w.count})`));
    if (w.hint) label.title = w.hint;
    li.appendChild(label);
    return li;
  }
  const btn = el('button');
  btn.type = 'button';
  btn.setAttribute('aria-expanded', w.collapsed ? 'false' : 'true');
  btn.append(chevron, el('span', '', w.name), el('span', 'cnt', `(${w.count})`));
  btn.title = w.hint || '';
  btn.onclick = () => {
    archivedOpen = !archivedOpen;
    // The fold is decided by the plan rather than by hiding rows, so the count, the tab order and the scroll
    // position all follow from one answer instead of from a class somebody remembered to toggle. Forgetting
    // what was drawn is how the whole list is redrawn, and this is the one change allowed to do it.
    lastWant = [];
    loadSessions();
  };
  li.appendChild(btn);
  return li;
}

// wireSessions is the filter box. It redraws on every keystroke: the list is already in memory, the
// comparison is three lowercase strings, and a filter that waits a beat before answering feels broken. What
// is not done here is any focus handling - `wireKeys` in app.js owns the `/` that comes here from anywhere
// else in the app, and this module does not get to decide what a key outside the box means.
export function wireSessions() {
  const box = $('#session-filter');
  // The top bar's way of letting go. It lives beside the session's own name because that is what the button
  // is about: the conversation leaves this window, and the row in the list stays exactly where it was.
  const down = $('#btn-unload');
  if (down) down.onclick = () => unloadSession();
  if (!box) return;
  box.oninput = () => loadSessions();
  box.onkeydown = (e) => {
    // Escape in the box means "finished filtering", and that is said more specifically than the Escape in
    // app.js answers: clearing the word and putting the caret back on the transcript is what a reader in the
    // box is asking for, twice over - once to clear a long word, once more to leave.
    if (e.key !== 'Escape') return;
    // The box owns Escape only while it has something to give back. An empty box is already saying "finished
    // filtering", so it steps out of the way - out of focus, and out of the event - and the app's own Escape
    // answers the rest (close the panel the box is standing in, else go to the newest message). Swallowing it
    // here is what a measurement caught: with the sidebar collapsed, Escape in the filter dropped focus and
    // left the panel open, so the one key the app promises for getting out of a floating layer had to be
    // pressed twice and the first press looked like nothing had happened.
    if (!box.value) {
      box.blur();
      return;
    }
    e.stopPropagation();
    box.value = '';
    loadSessions();
  };
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
  // why here and not in refresh(): this mirror is this tab's memory of what *it* sent, and it survives a
  // re-read of the same session (a refresh is caused by events, including the ones that do not touch the
  // queue). Changing session is the only thing that makes those copies somebody else's. The count that
  // trims it belongs to the session being loaded, which is why it goes empty with the entries.
  state.held = [];
  // And the "waiting on" badge: it belongs to the session being looked at, which is already `state.id` — so
  // this re-draws rather than clears, and a summariser that started while you were elsewhere is on the bar
  // the moment the session that is running one is selected.
  syncBusy();
  state.tools.clear();
  state.toolResults.clear();
  state.streams.clear();
  for (const [, card] of state.requests) card.remove();
  state.requests.clear();
  // reveal: this is a click on a session, and the row that was just clicked has to be where the list ends
  // up - a session chosen from a closed panel or from below the fold is otherwise a list that silently
  // disagrees with the transcript next to it.
  await loadSessions({ reveal: true });
  await refresh();
}

// putDown asks the daemon to let one session go (POST .../unload). A session nobody has open costs nothing to
// keep — the next read replays log.jsonl and finds the queue, so a parked tool wait survives it — and an LLM
// stream in flight cannot be orphaned, so a session that is actively running or summarizing is refused with an
// error naming its phase. `clearWindow` decides whether this tab forgets it too: yes for the session on screen,
// no for a row you are not looking at, where there is nothing on screen to clear. Releasing a session you are
// not viewing is the reason the button sits on every open row: the daemon opens every session at boot and,
// until now, put none of them down.
async function putDown(id, clearWindow) {
  let res;
  try {
    res = await api(`/api/session/${encodeURIComponent(id)}/unload`, { body: {} });
  } catch (err) {
    toast(err.message, 'error'); // the daemon kept it open; the window would be wrong to pretend otherwise
    return;
  }
  if (clearWindow) downWindow(id);
  loadSessions(); // the row redraws as shut: its dot says so and the button is not drawn again
  note(res.unloaded ? `put down ${id} — the next read of it re-opens it from disk` : `${id} was not open in the daemon`);
}

// unloadSession is the header button: put the session on screen down, in the daemon and in this window alike.
// The empty state is worth having because watching a long run and then wanting the screen back used to have
// exactly two answers, one of which deleted a session and the other of which was another browser tab.
export function unloadSession(id = state.id) {
  if (!id) {
    note('no session is open in this window');
    return;
  }
  putDown(id, id === state.id);
}

// downWindow puts the session down in this window: the transcript, the badges, the queue mirrors and the hash
// that names the conversation. Nothing here is destroyed — this half is a tab forgetting, not the daemon.
function downWindow(id) {
  state.id = null;
  state.cfg = null;
  state.win = null;
  state.pathIds = [];
  state.held = [];
  state.pending = [];
  state.status = null;
  state.attachments = [];
  emit(EV_SESSION, ''); // the tool drawer keeps a session open of its own; it is told before anything repaints
  writeHash(''); // the address bar describes what is on screen, and what is on screen is nothing
  logEl().textContent = '';
  state.entries.clear();
  state.tools.clear();
  state.toolResults.clear();
  state.streams.clear();
  for (const [, card] of state.requests) card.remove();
  state.requests.clear();
  resetScroll();
  clearBusy(id);
  renderPending();
  clearStatus();
}

// clearStatus empties the status bar. renderStatus has no "no session" branch and should not acquire one: the
// bar is a report about a session, and with no session the honest report is blank.
function clearStatus() {
  const phase = $('#phase');
  phase.textContent = '—';
  phase.className = 'badge';
  $('#turn').textContent = '';
  $('#tools').textContent = '';
  $('#tools').title = '';
  $('#ctx-text').textContent = '';
  $('#ctx-fill').style.width = '0%';
  $('#session-name').value = '';
  const wake = $('#wake');
  wake.textContent = '';
  wake.title = '';
  wake.classList.add('hidden');
}

// showBusy and clearBusy own the one badge in the status bar that answers "why is nothing moving". It is
// deliberately not a note: a note fades in nine seconds, and the thing it would explain — a summariser
// thinking — is the longest wait in this product and outlives the note. It is also not derived from the
// phase, because the phase cannot say it: an automatic compaction runs inside a turn and leaves the phase at
// `running` for the whole minute its summary is being written, which is exactly the freeze that reads as a
// hang. So whoever knows writes it (events.js, on a compaction stage) and three hands erase it: that
// compaction's own result, a phase that can no longer be busy (below), and looking at another session.
//
// It is remembered per session (state.busy) rather than being a property of the screen, because the wait
// outlives the tab's attention: the summariser of session B runs while you read session A, and the badge you
// want at the moment you click B is the one whose frame arrived and was thrown away. So the frame is filed
// under its own session, and whatever the screen shows is a re-draw of that file.
export function showBusy(text, title, forSession) {
  const id = forSession || state.id;
  if (id) state.busy.set(id, { text, title: title || '', at: Date.now() });
  syncBusy();
}

export function clearBusy(id) {
  const target = id || state.id;
  if (target) state.busy.delete(target);
  syncBusy();
}

// syncBusy paints whatever the session on screen is currently waiting on, and nothing else. Every caller
// that changes the badge's subject — a frame arriving, a select, a phase moving — ends here, so the badge
// and the session named in the status line can never disagree.
export function syncBusy() {
  const box = $('#busy');
  if (!box) return;
  const held = state.id && state.busy.get(state.id);
  if (!held) {
    box.textContent = '';
    box.title = '';
    box.classList.add('hidden');
    return;
  }
  box.textContent = held.text;
  box.title = held.title;
  box.classList.remove('hidden');
}

export function renderStatus(st) {
  if (!st) return;
  const phase = $('#phase');
  phase.textContent = st.phase;
  phase.className = 'badge ' + st.phase;
  // Nobody is owed a "why is this waiting" badge once the loop is neither answering nor summarising. This is
  // the safety net under showBusy: a stage line from a run that ended on the way (a killed summariser, a
  // provider that refused the payload) has no fact left to report, and a badge that outlives its run is a
  // story the daemon stopped telling. It retires the record rather than the box, so the same truth holds for
  // a session this tab is not looking at, and syncBusy is the only writer of the box.
  if (st.phase !== 'running' && st.phase !== 'compacting') state.busy.delete(st.session || state.id);
  syncBusy();
  // The composer's right-hand slot belongs to the phase: send while idle, the red square and the bolt
  // while a run is live, and the square alone while a manual summary is in flight (Stop is the only hand
  // on one). One owner for that decision (composer.js), because the glyph and the Enter key have to agree
  // about what a send is worth right now - and Enter always sends, queued by a busy session or not.
  syncComposer(st.phase === 'running', st.phase === 'compacting');
  // And the one fact `running` cannot carry: that this session is asleep on a tool call the daemon owns the
  // wake-up for (js/park). It is shown as the moment the wait is due, never as a countdown — nothing
  // re-renders while a session is asleep, and a figure that goes stale next to a promise is a lie about the
  // promise. The title is where the two things a reader might act on live: that a restart does not lose the
  // wait, and that it does lose anything sent while waiting.
  const wake = $('#wake');
  const due = st.wakeAt ? new Date(st.wakeAt) : null;
  if (due && !Number.isNaN(due.getTime())) {
    wake.textContent = `asleep → ${fmtTime(st.wakeAt)}`;
    wake.title =
      `A tool call is waiting until ${fmtFull(st.wakeAt)}.\n` +
      'The loop is not held open for it: the daemon starts the run that answers it a few seconds before that moment.\n' +
      'Restarting the daemon keeps the wait — it is recomputed from the log — but a message sent while it waits lives in memory, and a restart drops it.';
    wake.classList.remove('hidden');
  } else {
    wake.textContent = '';
    wake.title = '';
    wake.classList.add('hidden');
  }
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
  // The queue this status reports is drawn here rather than where the queue changes hands, because a queue
  // that is being held does not change hands and says nothing at all: `Send` takes the message into memory
  // and emits no event, and the only thing that ever mentions it is the next status. This is also how the
  // box learns a held message has finally joined the log — the same status that reports `queued: 0`.
  renderPending();
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

// HELD_SHOWN is smaller than PENDING_SHOWN because those are ids nobody reads and these are sentences
// somebody does, and this box shares its column with the transcript it must not crowd out (app.css).
const HELD_SHOWN = 4;

export function renderPending() {
  const box = $('#pending');
  const list = $('#pending-list');
  const badge = $('#pending-badge');
  list.textContent = '';
  if (!state.pending.length) {
    badge.classList.add('hidden');
  } else {
    badge.classList.remove('hidden');
    badge.textContent = `pending: ${state.pending.length}`;
    for (const id of state.pending.slice(0, PENDING_SHOWN)) list.appendChild(el('li', '', id));
    if (state.pending.length > PENDING_SHOWN) {
      list.appendChild(el('li', 'more', `+ ${fmtN(state.pending.length - PENDING_SHOWN)} more`));
    }
  }
  const held = renderHeld();
  // One box, two reasons to be open: something on the path waiting to be answered, or something off the
  // path waiting to be appended.
  box.classList.toggle('hidden', !state.pending.length && !held);
}

// renderHeld draws the messages the daemon is holding off the path and returns how many it is holding.
//
// It needs two sources because neither knows the whole story. The count is the server's (`status.queued`),
// the only figure that also covers a message another window sent; the text is this tab's (`state.held`),
// which the server never stores where a client could read it. What reconciles them is that the queue is
// FIFO — park.go appends held messages to the path in the order they arrived — so when the count drops, the
// ones that left are the oldest, and trimming the head of the mirror is arithmetic rather than a guess.
// Where the count is larger than the mirror, the difference is counted out loud and never invented: this
// tab cannot know words it never sent. Measured both ways: three sends from this tab drew three rows, and
// the same three after a reload drew none and one line reading "+ 3 more".
//
// When this runs is not "whenever the daemon's number moves". The page is event-driven (the poll in
// transport.js only exists while the websocket is down), and a sleeping or parked wait emits nothing while
// it sleeps. Measured: a send from a second client left this box on `held: 2` for the whole of a parked
// wait while the daemon was already counting 3, and the number came right by itself when the hold ended.
// A send from *this* tab does not have to wait for an event at all — composer.js takes the count off the
// POST's own answer (see rememberHeld) — which is why the message you just sent is in this box at once.
//
// The mirror is not cleared by the `entry` event that finally shows the message, because nothing in that
// event says which queued message it was, and a row drawn twice is a message that looks like it happened
// twice. It is cleared by the count reaching zero, which is the same fact with less ambiguity — at that
// point the entries have arrived, and one of the two renders is a frame's overlap rather than a claim.
// Measured as a stop: a parked run held three of them, and stopping it wrote all three (once each, oldest
// first) before the answer that reads them.
function renderHeld() {
  const badge = $('#held-badge');
  const list = $('#held-list');
  const queued = state.status?.queued || 0;
  list.textContent = '';
  if (!queued) {
    state.held = [];
    badge.classList.add('hidden');
    badge.title = '';
    return 0;
  }
  if (queued < state.held.length) state.held = state.held.slice(state.held.length - queued);
  badge.classList.remove('hidden');
  badge.textContent = `held: ${fmtN(queued)}`;
  badge.title = 'Not in the log yet. A tool call is still owed its answer and this message would land between that call and its result, so the daemon keeps it in memory and appends it once the answer is written — a restart drops these.';
  const unknown = Math.max(0, queued - state.held.length);
  for (const m of state.held.slice(0, HELD_SHOWN)) {
    const words = m.text.replace(/\s+/g, ' ').trim();
    const li = el('li', 'held-text', words || `(${m.images} image${m.images === 1 ? '' : 's'})`);
    li.title = [words, m.images ? `${m.images} image(s)` : ''].filter(Boolean).join(' + ');
    list.appendChild(li);
  }
  const hidden = state.held.length > HELD_SHOWN ? state.held.length - HELD_SHOWN : 0;
  if (hidden || unknown) {
    const li = el('li', 'more', `+ ${fmtN(hidden + unknown)} more`);
    // why a line and not a blank: the count is a fact even where the words are not, and "bareloopd is
    // holding 3" is the answer to the question the box exists for. Only the words are missing, and this
    // says which and how many rather than leaving the number unexplained.
    li.title = unknown
      ? `${fmtN(unknown)} of them did not come from this tab — another window, or sent before this page was loaded — so the words are not here.`
      : '';
    list.appendChild(li);
  }
  list.appendChild(el('li', 'why', 'not in the log yet · a restart drops these'));
  return queued;
}
