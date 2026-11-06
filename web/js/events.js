// Turning agent events into DOM. The transport hands every event here, whether it arrived over the
// websocket, SSE or a poll — there is exactly one rendering path either way.
import { el, fmtN, logEl, numId } from './util.js';
import { isParked, state } from './state.js';
import { note, toast } from './ui.js';
import { scrollDown } from './scroll.js';
import { claimStream, entryNode, requestCard, retireStream, streamDelta, toolCard } from './transcript.js';
import { renderTools, syncToolsPanel } from './tools.js';
import { clearBusy, loadSoon, refresh, renderNewer, renderPending, renderStatus, returnToTip, showBusy } from './session.js';

// Everything on this list answers "what is happening now" by drawing at the bottom of #log, and that
// sentence only means something while the bottom of #log is the newest message. A jump parks the
// transcript on a page that is not the end of the conversation: appending there would put tonight's
// answer in the middle of a conversation from last month, and the tip rule in the `state` case below
// would then drag the view back to the tip on the next frame. So a parked page stops believing them and
// counts what it gave up — `#newer` says so in its title, because a transcript that is being ignored
// looks perfectly calm. A reconnect still finds the page: `ws.onopen` refreshes, and a refresh asks for
// a parked page by its anchor. Notes are not on the list on purpose: a line about the daemon is not a
// claim about where this conversation is, and muting an error because a page wanted to look tidy is the
// worse trade — which is the reason the one error this file does mute is muted by what caused it, never
// by where it happened to land.
const LIVE_AT_THE_TAIL = ['delta', 'entry', 'tool_call', 'tool_result', 'compaction', 'removed', 'request', 'request_done'];

// What a run the reader stopped looks like on the wire. The event carries a message and a level and no
// field that says "this was a cancellation" — and a flag on this tab would miss every way a run is
// stopped that is not this tab's Stop button (another tab, curl, ⚡ interrupt) — so the cause is read off
// the text. Go's context.Canceled is a constant string, and the only two emitters a cancel can reach are
// the streaming call and the plain one, which prefix "llm stream: " and "llm: "; the wrapped form of a
// transport error puts the URL in between, hence the .* before the cause. Everything else keeps its red
// line: "panic in run:", "llm:" for a real provider failure, "commit tool result:" and "held message:"
// are all things worth seeing on a page that was merely being read.
const STOPPED_RUN = /^llm(?: stream)?: .*\bcontext canceled\b/;

export function handleEvent(ev) {
  if (ev.seq !== undefined) state.seq = Math.max(state.seq, ev.seq || 0);
  // Nothing is on screen (this window put its session down). The hub still sends everything, so the list is
  // told when a session moves and the transcript is left alone — drawing an entry into an empty window would
  // be putting somebody's conversation back on a screen that just asked for it back.
  if (!state.id) {
    if (ev.type === 'state' || ev.type === 'entry') loadSoon();
    // Filed even with nothing on screen: a session put down in this window is still a session the daemon is
    // running, and reading it again should not lose a minute of "why was it sitting on running".
    if (ev.type === 'compaction') fileCompaction(ev.session, ev.data);
    return;
  }
  if (ev.session && state.id && ev.session !== state.id) {
    // Another session produced this: the only thing on screen that cares is the sidebar.
    if (ev.type === 'state' || ev.type === 'entry') loadSoon();
    // …and the badge of a session nobody is looking at, which retires on the same rule renderStatus uses for
    // the one on screen. Without this a tab that stayed on session A for a day would keep claiming B was
    // still writing a summary it finished hours ago, and selecting B would flash that claim for a frame.
    if (ev.type === 'state' && ev.state && ev.state.phase !== 'running' && ev.state.phase !== 'compacting') clearBusy(ev.session);
    // A compaction frame about another session is filed instead of drawn. It is the one piece of news about a
    // session you are not looking at that you want the instant you click it: "why is this one sitting on
    // running" is asked at the click, and the frame answering it was broadcast a minute earlier while this
    // tab was busy with a different conversation.
    if (ev.type === 'compaction') fileCompaction(ev.session, ev.data);
    return;
  }
  if (isParked() && LIVE_AT_THE_TAIL.includes(ev.type)) {
    state.win.stale = (state.win.stale || 0) + 1;
    // D99: parked, a bubble at the tail is not the reply's body, it is a preview of news this page declined
    // to draw, and an entry naming a live stream is the daemon saying that preview's reply is in the log now.
    // So it goes — and dropping it is not drawing at the tail, it is the opposite: a preview left behind is
    // re-hung by the next rebuild and sits there claiming to be an entry the window already has (the resident
    // version of the two-bubbles bug D97 exists to prevent). The page's tally is untouched: the reply is still
    // news this page refused, and `#newer` still says how many it refused.
    if (ev.type === 'entry' && ev.streamId) retireStream(state.streams.get(ev.streamId));
    // Counting is not telling: this chip is the only place those words live, and nothing else rewrites it
    // while parked — a parked page is exactly the one that does not refresh. So the tally would sit there
    // computed, and a transcript being quietly ignored would still look perfectly calm.
    renderNewer();
    return;
  }
  switch (ev.type) {
    case 'hello':
      break;
    case 'delta':
      streamDelta(ev.delta);
      break;
    case 'entry':
      onEntry(ev.entry, ev.streamId);
      break;
    case 'removed':
      state.entries.get(ev.info)?.remove();
      state.entries.delete(ev.info);
      break;
    case 'state': {
      const prevTip = state.status?.tip;
      state.status = ev.state;
      renderStatus(ev.state);
      // Coalesced for the same reason the other sessions' events are: a running session pushes a state event
      // per tick, and the sidebar's numbers are a snapshot rather than a stream.
      loadSoon();
      // A tip that moved backwards is a time travel or a deleted entry: what is on screen is no longer
      // the path, and no single event can fix that. A tip that moved forwards to an entry nobody is
      // holding means the path changed somewhere this tab did not watch — another tab sent a message.
      // A forward move to an entry already on screen is this session's own work, already drawn.
      // Parked, both rules change shape: a forward move is what the "newer" chip is for rather than a
      // reason to re-read a page that did not change, and a backwards move means the entry this page was
      // anchored on may have stopped existing — so it goes back to the tip, and says so where a rebuild
      // cannot eat the message.
      const tip = ev.state?.tip;
      const parked = isParked();
      const moved = !!tip && !!prevTip && tip !== prevTip;
      if (moved && numId(tip) < numId(prevTip)) {
        if (parked) {
          toast('this conversation went backwards while you were reading a page of it — back to the newest message.');
          returnToTip().catch(() => {});
        } else refresh().catch(() => {});
      } else if (moved && !parked && !state.entries.has(tip)) refresh().catch(() => {});
      syncToolsPanel();
      break;
    }
    case 'tools':
      // The catalogue is pushed whole whenever the published set changes, which is how a second tab
      // learns about a switch flipped in the first one.
      renderTools(ev.data);
      break;
    case 'pending':
      state.pending = ev.pending || [];
      renderPending();
      break;
    case 'tool_call':
      onToolProgress(ev.run);
      break;
    case 'tool_result':
      onToolProgress(ev.run, true);
      break;
    case 'compaction':
      onCompaction(ev.data);
      break;
    case 'request':
      requestCard(ev);
      break;
    case 'request_done':
      state.requests.get(ev.info)?.remove();
      state.requests.delete(ev.info);
      break;
    case 'log':
      // Two producers use this frame: the daemon logger sends {data:{line}}, a session talking about
      // itself sends {info, level} — a tool name collision is the second kind, and it must not fade
      // out in nine seconds the way chatter does, because the user needs it while reading the drawer.
      note(ev.info || ev.data?.line || '', ev.level === 'warn' ? 'warn' : 'note');
      break;
    case 'info':
      note(ev.info || '');
      break;
    case 'error':
      // A reader who pressed Stop does not need a red line telling them the run stopped: the transcript
      // already says "interrupted" on the entry the daemon committed for exactly that. See STOPPED_RUN.
      if (!STOPPED_RUN.test(ev.info || '')) note(ev.info || 'error', 'error');
      break;
    default:
      break;
  }
}

// onEntry draws one committed entry. `streamId` is the second argument because an assistant entry is usually
// not new information — it is the same message the reader has been watching arrive, now with the log's
// authority behind it — and the event names the live buffer it is the commit of rather than leaving the view
// to guess which one that was.
function onEntry(e, streamId) {
  if (!e) return;
  // The buffer this reply was streamed out on, named by the daemon (`agent.Event.StreamID`) rather than
  // inferred: an entry that was streamed carries the id of its stream and one that was not carries nothing
  // (a user message, a tool result, a compaction, a provider that never streamed), which is the whole rule —
  // no name means there is no buffer to take over. Guessing was worse than it looked: "the newest buffer must
  // be this reply's" is a claim about ordering, and the closing `done` frame arrives after the entry, so the
  // buffer is still open at this moment and holds nothing that says which entry it belongs to. It was also
  // never even comparing what it meant to — `[...map]` is an array of key/value *pairs*, so the buffer object
  // was tested against a pair, only a reply with tool calls could still be recognised (by its call ids), and
  // every plain answer left its bubble standing next to the committed one.
  const s = streamId ? state.streams.get(streamId) : null;
  if (state.entries.has(e.id)) {
    // Already on screen: a rebuild read the log while this answer was still arriving, so what the transcript
    // is holding is the committed rendering of it. That node stands — it came from the log — and the bubble
    // has nothing left to say, so it goes. Retiring it is also what silences the `done` frame still to come.
    if (s) retireStream(s);
    return;
  }
  if (s && e.kind === 'assistant') claimStream(s, e);
  else entryNode(e);
  if (e.kind === 'user') {
    state.pending = state.pending.filter((p) => p !== e.id);
    renderPending();
  }
  // A committed compaction changes which entries are on the path, so the whole transcript is redrawn
  // rather than appending one row.
  if (e.kind === 'compaction') refresh().catch(() => {});
  scrollDown();
}

function onToolProgress(d, isResult) {
  if (!d) return;
  const card = toolCard(d.callId, `${d.server ? d.server + '/' : ''}${d.tool}`, d.status === 'error' ? 'state-error' : 'state-running');
  // A card drawn from the stream's own progress frame can only name the tool: ToolCallProgress carries no
  // server. After a reload the transcript is drawn from the committed entry, which does carry one — so this
  // event, the first frame that knows who owns the call, is the one that relabels. Without it the same call
  // reads `fs_list` while it runs and `mcp-vroot/fs_list` once the page is redrawn from the log.
  if (d.server) card.setLabel(`${d.server}/${d.tool}`);
  if (d.arguments) card.setArgs(d.arguments);
  if (d.status === 'running') card.setBadge('running', 'state-running');
  // The output is not drawn from this event any more: the tool entry owns it, renders it in full (images
  // included) and can be replayed, which this frame never could. What the frame still knows first is that
  // the call ended, so the badge comes from here; the entry's own event binds the card to it and wins,
  // because it carries the milliseconds the tool itself measured.
  if (isResult) {
    // A parked handover arrives as a tool_result and carries an `ms`, so without this branch the card comes
    // up “12 ms ✓” — the badge of a call that finished quickly, on the one call in this product that has not
    // finished at all and may not for another six hours. The tooltip is the server's own sentence about the
    // handover, so the card cannot disagree with what the daemon promised.
    if (d.status === 'parked') card.setBadge('asleep', 'state-parked', d.result || 'this wait is scheduled and the loop is not held open for it');
    else if (d.status === 'error') card.setBadge('error', 'state-error');
    else if (d.ms) card.setBadge(`${d.ms} ms`, 'state-ok');
  }
  attach(card, d.entryId);
}

// attach hangs a card under the entry it belongs to, or at the end of the transcript when that entry
// is not on screen (a progress event can beat the entry that owns it).
function attach(card, entryId) {
  if (card.attached) return;
  const target = state.entries.get(entryId);
  (target || logEl()).appendChild(card.el);
  card.attached = true;
}

// BUSY_TITLE is the sentence the badge points at: what a summariser is, why nothing else is moving, and the
// one control that ends the wait. It lives here because this event is the only thing in the product that
// knows a summary is being written, and it is the answer a reader types into a search box about a session
// that looks hung.
const BUSY_TITLE =
  'The context is over the compaction trigger, so this turn is being summarised before it can be sent — or the summary just came back over its own cap and is being asked again.\n\nNothing else is happening while this runs: the answer you asked for waits behind it, and anything you type is queued until it lands. The summariser can take a minute per attempt.\n\nStop (the red square, POST /api/session/<id>/stop) is the only hand on it: it kills the summariser, writes nothing, and leaves the conversation as it was.';

// fileCompaction records what one session is waiting on, under that session's id. Called for the session on
// screen and — from the cross-session branch above — for the ones nobody is looking at, so selecting a
// session that has been writing a summary for a minute shows the badge instead of a phase word and silence.
function fileCompaction(id, r) {
  if (!id || !r) return;
  if (r.stage) showBusy(`writing the summary${r.attempt ? ` · attempt ${r.attempt}` : ''}…`, BUSY_TITLE, id);
  else clearBusy(id);
}

// onCompaction reports one compaction run: progress lines while it works, one banner when it ends.
export function onCompaction(r) {
  if (!r) return;
  if (r.stage) {
    // The one moment a session looks broken and is not: the summariser is answering, no token of the
    // conversation is moving, and the phase still says `running` because the compaction is inside the run.
    // The note stays (it is the attempt-by-attempt record a reader scrolls back through), but a note fades and
    // this wait does not, so the badge is what actually answers "is it hung". Stop is the way out, and the
    // badge says so rather than letting a reader conclude the page died.
    fileCompaction(state.id, r);
    note(`compaction: ${r.stage}${r.attempt ? ` (attempt ${r.attempt})` : ''}`);
    return;
  }
  // Every answer below is the end of the wait the badge was standing for — committed, refused, or never
  // needed. The summary entry arrives separately (and asks for a rebuild itself); this event is the last
  // word on whether anything is still in flight.
  clearBusy();
  if (r.noop) {
    note(`compaction: ${r.skipped || 'nothing to do'}`);
    return;
  }
  const box = el('div', 'compact-banner');
  const head = r.committed ? 'compacted' : r.skipped ? `compaction skipped (${r.skipped})` : 'compaction not accepted';
  box.appendChild(el('div', '', `${head} · ${r.trigger || 'manual'}${r.manual ? ' (manual)' : ''}${r.gate ? ` · gate: ${r.gate}` : ''}`));
  // The `before → after` row is a measurement of a change, and an aborted compaction changed nothing.
  // `after` is only ever assigned when the summary entry is written, so printing it otherwise renders
  // "context 6.9k → 0" — the one outcome §1.1 promises never happens, asserted by the very line that is
  // trying to report a failure. An abort owes the reader two things: that nothing was written, and why.
  // The head line above carries the reason (`compaction skipped (<why>)`), so this row carries the first.
  if (r.committed) {
    box.appendChild(
      el(
        'div',
        '',
        `context ${fmtN(r.before)} \u2192 ${fmtN(r.after)} · replaced ${r.replaced ?? 0} entries · summarised ${fmtN(r.summarizeTokens)} \u2192 kept ${fmtN(r.keptTokens)} · attempts ${r.attempts ?? 0}${r.empty ? ' · context emptied' : ''}${r.split ? ' · split turn' : ''}`,
      ),
    );
  } else {
    box.appendChild(el('div', '', `nothing was written — the history still holds ${fmtN(r.before)} tokens of context`));
  }
  // Same rule as the row above: a boundary that was never written is not a fact about the path.
  if (r.committed && r.boundary) box.appendChild(el('div', '', `boundary ${r.boundary}${r.entryId ? ` · entry ${r.entryId}` : ''}`));
  logEl().appendChild(box);
  if (r.committed) refresh().catch(() => {});
  scrollDown();
}
