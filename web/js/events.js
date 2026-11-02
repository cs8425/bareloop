// Turning agent events into DOM. The transport hands every event here, whether it arrived over the
// websocket, SSE or a poll — there is exactly one rendering path either way.
import { el, fmtN, logEl, numId } from './util.js';
import { isParked, state } from './state.js';
import { note, toast } from './ui.js';
import { scrollDown } from './scroll.js';
import { entryNode, requestCard, streamDelta, toolCard } from './transcript.js';
import { renderTools, syncToolsPanel } from './tools.js';
import { loadSoon, refresh, renderNewer, renderPending, renderStatus, returnToTip } from './session.js';

// Everything on this list answers "what is happening now" by drawing at the bottom of #log, and that
// sentence only means something while the bottom of #log is the newest message. A jump parks the
// transcript on a page that is not the end of the conversation: appending there would put tonight's
// answer in the middle of a conversation from last month, and the tip rule in the `state` case below
// would then drag the view back to the tip on the next frame. So a parked page stops believing them and
// counts what it gave up — `#newer` says so in its title, because a transcript that is being ignored
// looks perfectly calm. A reconnect still finds the page: `ws.onopen` refreshes, and a refresh asks for
// a parked page by its anchor. Notes are not on the list on purpose: a line about the daemon is not a
// claim about where this conversation is, and muting an error to keep a page tidy is the worse trade.
const LIVE_AT_THE_TAIL = ['delta', 'entry', 'tool_call', 'tool_result', 'compaction', 'removed', 'request', 'request_done'];

export function handleEvent(ev) {
  if (ev.seq !== undefined) state.seq = Math.max(state.seq, ev.seq || 0);
  if (ev.session && state.id && ev.session !== state.id) {
    // Another session produced this: the only thing on screen that cares is the sidebar.
    if (ev.type === 'state' || ev.type === 'entry') loadSoon();
    return;
  }
  if (isParked() && LIVE_AT_THE_TAIL.includes(ev.type)) {
    state.win.stale = (state.win.stale || 0) + 1;
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
      onEntry(ev.entry);
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
      note(ev.info || 'error', 'error');
      break;
    default:
      break;
  }
}

function onEntry(e) {
  if (!e) return;
  if (state.entries.has(e.id)) return;
  const node = entryNode(e);
  if (e.kind === 'assistant') {
    // The committed entry supersedes the live stream buffer: move the streamed tool cards into the
    // committed message and drop the buffer, so the tool cards are not rendered twice.
    for (const [id, s] of [...state.streams]) {
      for (const [, card] of s.tools) {
        card.attached = true;
        node.appendChild(card.el);
      }
      s.node.remove();
      state.streams.delete(id);
    }
  }
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

// onCompaction reports one compaction run: progress lines while it works, one banner when it ends.
export function onCompaction(r) {
  if (!r) return;
  if (r.stage) {
    note(`compaction: ${r.stage}${r.attempt ? ` (attempt ${r.attempt})` : ''}`);
    return;
  }
  if (r.noop) {
    note(`compaction: ${r.skipped || 'nothing to do'}`);
    return;
  }
  const box = el('div', 'compact-banner');
  const head = r.committed ? 'compacted' : r.skipped ? `compaction skipped (${r.skipped})` : 'compaction not accepted';
  box.appendChild(el('div', '', `${head} · ${r.trigger || 'manual'}${r.manual ? ' (manual)' : ''}${r.gate ? ` · gate: ${r.gate}` : ''}`));
  box.appendChild(
    el(
      'div',
      '',
      `context ${fmtN(r.before)} \u2192 ${fmtN(r.after)} · replaced ${r.replaced ?? 0} entries · summarised ${fmtN(r.summarizeTokens)} \u2192 kept ${fmtN(r.keptTokens)} · attempts ${r.attempts ?? 0}${r.empty ? ' · context emptied' : ''}${r.split ? ' · split turn' : ''}`,
    ),
  );
  if (r.boundary) box.appendChild(el('div', '', `boundary ${r.boundary}${r.entryId ? ` · entry ${r.entryId}` : ''}`));
  logEl().appendChild(box);
  if (r.committed) refresh().catch(() => {});
  scrollDown();
}
