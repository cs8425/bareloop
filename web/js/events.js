// Turning agent events into DOM. The transport hands every event here, whether it arrived over the
// websocket, SSE or a poll — there is exactly one rendering path either way.
import { el, fmtN, logEl } from './util.js';
import { state } from './state.js';
import { note } from './ui.js';
import { scrollDown } from './scroll.js';
import { entryNode, requestCard, streamDelta, toolCard } from './transcript.js';
import { loadSessions, refresh, renderPending, renderStatus } from './session.js';

export function handleEvent(ev) {
  if (ev.seq !== undefined) state.seq = Math.max(state.seq, ev.seq || 0);
  if (ev.session && state.id && ev.session !== state.id) {
    // Another session produced this: the only thing on screen that cares is the sidebar.
    if (ev.type === 'state' || ev.type === 'entry') loadSessions();
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
    case 'state':
      state.status = ev.state;
      renderStatus(ev.state);
      loadSessions();
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
  if (!isResult) {
    attach(card, d.entryId);
    return;
  }
  const images = d.images ? ` \u00b7 ${d.images} image(s) in the tool message` : '';
  card.setResult((d.error || d.result || '') + images, d.status === 'error');
  if (d.ms) card.setBadge(`${d.ms} ms`, d.status === 'error' ? 'state-error' : 'state-ok');
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
