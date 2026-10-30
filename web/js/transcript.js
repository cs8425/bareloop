// Drawing the transcript: committed entries, live streams, tool cards, and the two kinds of pending
// input request. Everything here renders from the events the server sends; none of it decides
// anything. Refresh requests go out on the bus so this module does not import the session module.
import md from './md.js';
import { makeRenderGate, resolveStreamMode } from './scheduler.js';
import { $, el, collapseDataUrls, fmtN, logEl, pretty, whoName, tps, MIN_UNCACHED } from './util.js';
import { state } from './state.js';
import { call } from './api.js';
import { confirmBox, note } from './ui.js';
import { markPinned, scrollDown } from './scroll.js';
import { emit, EV_REFRESH } from './bus.js';
import { icon, swapIcon } from './icons.js';

// Branching is switched off in the UI, and only in the UI. POST /tip still works and the log still holds a
// tree — what is gone is the button that moves you onto another branch. Without a tree view there is no way
// to see which branches exist or to get back to one, and a button that quietly creates one is a trap: you
// would be typing at the end of a path you cannot find again. The transcript therefore draws the current
// path only. Re-enabling means the tree UI, not just this constant (docs/design.md, "Deferred").
const BRANCH_UI = false; // "branch here": a tip move with nothing to run

// Restart stays on, because it is not a way of browsing: it is how a run that stopped half way through the
// agent loop gets picked up again — a tool result that arrived with no answer after it, an output that died
// mid-reasoning. It moves the path to the last legal boundary and runs, appending nothing of its own.
const RESTART_UI = true; // "run again from here": tip move + POST /resume

// resumeFrom says whether a run can be picked up again at this entry, and what that would mean, in words.
// A request may only end on a user message, a tool result or a compaction summary; a half-finished or
// failed answer is not such a boundary — its parent is — which is exactly the case this exists for: the
// answer stopped mid-reasoning, so run again from before it. A finished answer is not offered, because
// "continue on from your own last answer" is a different and much vaguer question (say that in the box).
//
// A compaction entry is a do-over of another kind: it is not a request, it is the result of one. Running it
// again therefore means making that summary again from the messages that produced it - the path moves back
// to the summary's parent (with the summary off the path the conversation is exactly what it was when the
// compaction ran) and a fresh summary is asked for with whatever the compaction would use today. What had
// followed the old summary leaves the path. Then the rule is the one the loop already follows: what was
// running keeps running, what had stopped stays stopped - a summary written in the middle of a run has a run
// to carry on, one written after the model had finished its turn, or by hand while idle, has nothing waiting
// and must not start a turn nobody asked for. A summary that is still the end of the path has nothing to
// redo, so there the same distinction only decides whether to ask with it at all (a forced compaction ends a
// run with "the context was compacted, please send again" - that one is waiting).

// The trigger that wrote a summary is kept in its label, and it is the only trace of whether a run was in
// progress: "in_loop" is written before sending, inside a run - including the forced compaction after a
// provider refused the payload - while "end_of_turn" follows a finished turn and "manual" is the button.
const midLoop = (e) => (e.label || '').includes('in_loop');

function resumeFrom(e) {
  if (e.kind === 'user') return { anchor: e.id, why: 'ask this message again' };
  if (e.kind === 'tool') return { anchor: e.id, why: 'send this tool result again and let the model think' };
  if (e.kind === 'compaction') {
    if (!e.parentId) return { anchor: '', why: '' };
    if (e.id === state.status?.tip) {
      return midLoop(e) ? { anchor: e.id, why: 'ask again with this summary' } : { anchor: '', why: '' };
    }
    return {
      mode: 'recompact',
      anchor: e.parentId,
      why: midLoop(e)
        ? 'make this summary again from the messages before it and let the run continue'
        : 'make this summary again from the messages before it',
    };
  }
  const broken = e.interrupted || e.finishReason === 'error' || (e.label || '').includes('error');
  if (e.kind === 'assistant' && broken && e.parentId) {
    return { anchor: e.parentId, why: 'this answer stopped half way: run again from before it' };
  }
  return { anchor: '', why: '' };
}

export function entryNode(e) {
  let node = state.entries.get(e.id);
  if (node) return node;
  // A committed failure (provider error, stream that died halfway) is drawn as a failure: an
  // assistant message that says "[generation failed: …]" in normal colours looks like an answer.
  const bad = e.finishReason === 'error' || (e.label || '').includes('error');
  node = el('div', `msg ${e.kind}${e.interrupted ? ' interrupted' : ''}${bad ? ' error' : ''}`);
  const head = el('header');
  head.appendChild(el('span', 'who', whoName(e)));
  head.appendChild(el('span', 'eid', e.id));
  if (e.model) head.appendChild(el('span', 'eid', e.model));
  if (e.tool) head.appendChild(el('span', 'eid', `${e.server ? e.server + '/' : ''}${e.tool}`));
  if (e.isError) head.appendChild(el('span', 'state-error', 'error'));
  if (bad && !e.isError) head.appendChild(el('span', 'state-error', 'failed'));
  if (e.interrupted) head.appendChild(el('span', 'state-running', 'interrupted'));
  // Two groups of two numbers, and that is all this line gets: on a phone the same header used to wrap
  // into three lines. The first group says what the request moved (prompt, which includes the cached
  // part, over what it generated), the second how fast each half was. Everything else about the request
  // is one hover away, and the session-wide view is the cost panel behind the token bar.
  for (const chip of usageChips(e)) head.appendChild(chip);
  if (e.finishReason) head.appendChild(el('span', 'eid', e.finishReason));
  if (e.ts) {
    // A clock time down to the second was the widest and least used thing on the line. HH:MM is a fifth
    // of the width (always 24-hour, so it never depends on whose locale loaded the page), and the full
    // timestamp — with the zone, which is what you actually want next to a log entry — is in the title.
    const when = el('span', 'eid', new Date(e.ts).toTimeString().slice(0, 5));
    when.title = new Date(e.ts).toLocaleString();
    head.appendChild(when);
  }
  const tools = el('span', 'eid');
  tools.style.marginLeft = 'auto';
  tools.appendChild(actions(e, node));
  head.appendChild(tools);
  node.appendChild(head);

  if (e.reasoning) node.appendChild(reasoningBlock(e.reasoning));

  const body = el('div', 'body' + (e.kind === 'assistant' ? ' md' : ''));
  fillBody(node, body, e);
  node.appendChild(body);

  state.entries.set(e.id, node);
  logEl().appendChild(node);
  return node;
}

// ---------------------------------------------------------------- one request's numbers

// usageChips renders one request as `3.2k / 21 tokens` and `1494 / 55 tok/s`. An entry that carried no
// usage at all (your message, a tool result) keeps only its wall-clock duration, as before.
//
// A dash means "there is no honest number here", and the title says which of the two reasons applies:
// nothing was measured (an entry written before the server reported timings), or too little was —
// dividing a warm request's handful of uncached tokens by the fixed cost of a call reads as 33 tok/s
// and describes the server no better than a stopwatch on the network would.
function usageChips(e) {
  const u = e.usage;
  if (!u || (!u.prompt && !u.completion)) return e.ms ? [el('span', 'eid', `${e.ms} ms`)] : [];

  const cached = u.cache || 0;
  const fresh = Math.max((u.prompt || 0) - cached, 0);
  const inTps = tps(fresh, u.promptMs, MIN_UNCACHED);
  const outTps = tps(u.completion || 0, u.completionMs, 4);

  const details = [];
  details.push(u.prompt ? `prompt ${u.prompt}${cached ? ` — ${cached} cached, ${fresh} new` : ''}` : 'prompt not reported');
  details.push(u.completion ? `generated ${u.completion}` : 'nothing generated');
  details.push(`prefill ${u.promptMs ? Math.round(u.promptMs) + ' ms' : 'not reported'} · decode ${u.completionMs ? Math.round(u.completionMs) + ' ms' : 'not reported'}`);
  if (u.draftN) details.push(`speculative decoding: ${u.draftAccepted}/${u.draftN} draft tokens accepted`);
  if (e.ms) details.push(`${e.ms} ms end to end`);
  details.push('the token bar in the status bar adds these up for the whole conversation');
  const tokens = el('span', 'eid', `${fmtN(u.prompt || 0)}/${fmtN(u.completion || 0)} tokens`);
  tokens.title = details.join('\n');

  const chips = [tokens];
  // No timings at all means no rate chip either: an entry from an older server would spend twelve
  // characters on a permanent dash.
  if (u.promptMs || u.completionMs) {
    const why = [];
    if (inTps === null) why.push(fresh < MIN_UNCACHED ? `no prefill speed: ${fresh} new tokens were computed, below the ${MIN_UNCACHED} a measurement needs` : 'no prefill speed: the server reported no prompt time');
    if (outTps === null) why.push((u.completion || 0) < 4 ? `no decode speed: ${u.completion || 0} tokens is too few for the number to mean anything` : 'no decode speed: the server reported no generation time');
    const rate = el('span', 'eid', `${inTps ?? '—'}/${outTps ?? '—'} tok/s`);
    rate.title = `prefill / decode, for this request alone${why.length ? `:\n${why.join('\n')}.` : '.'}`;
    chips.push(rate);
  }
  return chips;
}

// ---------------------------------------------------------------- one message's actions
//
// Icons, with the words in their titles. This row is repeated on every message in the transcript, so
// spelled-out buttons would be the loudest thing on the screen; the accessible name still says what the
// glyph means, because "the square that stops it" has to be legible before it has ever been clicked.
function actions(e, node) {
  const row = el('span', 'acts');
  const act = (name, title, fn, when = true) => {
    if (!when) return null;
    const b = el('button', 'act');
    b.appendChild(icon(name));
    b.title = title;
    b.setAttribute('aria-label', title);
    b.onclick = (ev) => {
      ev.stopPropagation();
      fn(b);
    };
    row.appendChild(b);
    return b;
  };

  act('copy', 'copy this message', async (b) => {
    const ok = await copyText(e.content || '');
    if (ok) swapIcon(b, 'check', 'copy');
    else note('nothing was copied — the browser refused clipboard access');
  }, !!e.content);

  act('raw', 'view the raw text of this message', (b) => toggleRaw(e, node, b), true);

  act(
    'branch',
    'branch here — make this entry the end of the path (the entries after it survive, they are just not sent)',
    async () => {
      await call('/tip', { id: e.id });
      emit(EV_REFRESH);
    },
    BRANCH_UI, // off until there is a tree view to come back to
  );

  // Run again from here: the path moves to the last entry that may legally end a request and the loop
  // starts without appending anything — the tool result that already arrived is reused rather than redone,
  // and no duplicate user message is written. This is the recovery for a loop that stopped half way.
  const here = RESTART_UI && state.status?.phase !== 'running' ? resumeFrom(e) : { anchor: '', why: '' };
  act(
    'restart',
    here.anchor
      ? here.mode === 'recompact'
        ? `re-make the summary in ${e.id} — ${here.why}`
        : `run again from ${here.anchor} — ${here.why}`
      : '',
    async () => {
      const remake = here.mode === 'recompact';
      const idx = state.pathIds.indexOf(here.anchor);
      const after = idx >= 0 ? state.pathIds.slice(idx + 1) : null; // null: this path has not been drawn yet
      if (after === null || after.length || here.anchor !== state.status?.tip) {
        const yes = await confirmBox(remake ? `re-make the summary in ${e.id}?` : `run again from ${here.anchor}?`, {
          title: remake ? 're-make this summary' : 'run again from here',
          detail:
            (remake
              ? `The messages before it are summarised again${midLoop(e) ? ', and the run continues from the new summary' : ''}. `
              : '') +
            (after && !after.length
              ? `The path already ends at ${here.anchor}, so nothing is given up.`
              : after
                ? `${after.length} ${after.length === 1 ? 'entry' : 'entries'} after ${here.anchor} (${after[0]}…${after.at(-1)}) leave the path and will not be sent. They stay in the log; there is no way back to them from here.`
                : `The path moves to ${here.anchor}; whatever follows it today is given up (it stays in the log).`),
          ok: remake ? 're-make it' : 'run again',
          danger: !!(after === null || (after && after.length)),
        });
        if (!yes) return;
      }
      markPinned(); // following the new answer is the whole point of asking again
      try {
        if (here.anchor !== state.status?.tip) await call('/tip', { id: here.anchor });
        if (remake) {
          // One compaction request, then - only if this summary was written in the middle of a run - the run
          // it had been standing in the way of. The old entry's prompt is not carried over: the same trigger
          // is asked for the summary the same way it would be today. What had stopped stays stopped.
          await call('/compact', {});
          if (midLoop(e)) await call('/resume', {});
        } else {
          await call('/resume', {});
        }
      } finally {
        emit(EV_REFRESH); // the tip may have moved even when the run refused to start
      }
    },
    !!here.anchor,
  );

  act(
    'trash',
    'delete this entry',
    async () => {
      const yes = await confirmBox(`delete ${e.id}?`, {
        title: 'delete entry',
        detail: 'The entry is marked deleted in the log and its children are re-parented — history around it survives.',
        ok: 'delete',
        danger: true,
      });
      if (!yes) return;
      await call('/entry/delete', { id: e.id });
    },
  );

  return row;
}

// copyText puts the synchronous path first on purpose. It is not legacy decoration: this UI is normally
// opened as http://192.168.x.x:8090 (a phone, another box), which is not a secure context, so
// `navigator.clipboard` simply does not exist there - and where it does exist, its promise can sit
// unresolved until the browser decides the click was real, which would leave a copy button that never
// answers. The textarea trick is synchronous, works over plain http, and settles the question at once;
// the API is the fallback, and it is raced against a timeout so a stalled clipboard reports a failure
// instead of silence.
async function copyText(text) {
  const ta = el('textarea');
  ta.value = text;
  ta.style.position = 'fixed';
  ta.style.top = '-1000px';
  ta.style.opacity = '0';
  document.body.appendChild(ta);
  ta.select();
  let ok = false;
  try {
    ok = document.execCommand('copy');
  } catch {
    ok = false;
  }
  ta.remove();
  if (ok) return true;
  try {
    if (navigator.clipboard?.writeText) {
      await Promise.race([
        navigator.clipboard.writeText(text),
        new Promise((_, reject) => setTimeout(() => reject(new Error('clipboard timed out')), 800)),
      ]);
      return true;
    }
  } catch {
    // Nothing else to try: the caller says so out loud rather than failing quietly.
  }
  return false;
}

// toggleRaw swaps a message between what the model asked for and what the model said: the rendered
// markdown on one side, the stored text untouched on the other. Raw shows text only - an image is the
// one thing that cannot be shown as raw without shouting 400 KB of base64 across the screen, and
// transcript already lists its name and size above the text.
function toggleRaw(e, node, b) {
  const body = node.querySelector(':scope > .body');
  if (!body) return;
  const on = node.classList.toggle('raw');
  if (on) {
    body.textContent = '';
    body.appendChild(el('pre', 'raw-text', e.content || '(no text in this entry)'));
  } else {
    fillBody(node, body, e);
  }
  b.title = on ? 'show the rendered message again' : 'view the raw text of this message';
}

function fillBody(node, body, e) {
  body.textContent = '';
  // Attachments first, in the order they were stored — the same order the assembler puts on the
  // wire, so what is on screen is what the model was given.
  for (const p of e.parts || []) {
    if (p.type === 'image') {
      if (!p.data) {
        // No bytes, only a reference: show the reference. A broken image icon says less than a
        // path does.
        body.appendChild(el('div', 'attach-path', p.name || '[image with no data in the log]'));
        continue;
      }
      const mime = p.mimeType || 'image/png';
      const img = el('img');
      img.className = 'attach';
      img.src = `data:${mime};base64,${p.data}`;
      img.alt = p.name || 'image';
      img.title = `${p.name || 'image'} · ${mime} · ~${Math.round((p.data.length * 3) / 4 / 1024)} KB`;
      body.appendChild(img);
      continue;
    }
    if (p.text) body.appendChild(el('div', 'part-text', p.text));
  }
  if (e.kind === 'compaction') {
    body.classList.add('compact');
    const c = e.compaction || {};
    body.appendChild(
      el('div', '', `summary of ${c.replaced ?? '?'} entries → ${fmtN(c.summarySize)} tokens · boundary ${c.boundaryId || '-'} · model ${c.model || '-'}`),
    );
    if (c.broken) body.appendChild(el('div', 'state-error', 'boundary missing: the cut fell on a tool message, so this summary is not spliced in'));
    const pre = el('pre');
    pre.textContent = e.content || '';
    body.appendChild(pre);
    node.classList.add('compact-banner');
    return;
  }
  if (!e.content && !(e.toolCalls || []).length) return;
  if (e.kind === 'assistant') {
    if (e.content) md.renderTo(body, e.content);
    // Replay the call arguments so a reloaded page still shows what was passed.
    for (const tc of e.toolCalls || []) {
      const card = toolCard(tc.id, `${tc.server ? tc.server + '/' : ''}${tc.name}`, '');
      card.setArgs(typeof tc.arguments === 'string' ? tc.arguments : JSON.stringify(tc.arguments ?? ''));
      card.el.removeAttribute('open');
      body.appendChild(card.el);
    }
  } else if (e.kind === 'tool') {
    const pre = el('pre');
    pre.textContent = collapseDataUrls(e.content).text;
    body.appendChild(pre);
  } else {
    body.appendChild(document.createTextNode(collapseDataUrls(e.content).text));
  }
}

function reasoningBlock(text) {
  const d = el('details', 'reasoning');
  d.appendChild(el('summary', '', 'reasoning'));
  d.appendChild(document.createTextNode(text));
  return d;
}

// ------------------------------------------------------------------ live streams

// finishedStreams remembers streams that already ended. A reconnect replays the hub's recent events,
// and a replayed delta of a committed message would otherwise render the same reply twice.
const finishedStreams = new Set();

function markStreamFinished(id) {
  finishedStreams.add(id);
  if (finishedStreams.size > 200) finishedStreams.delete(finishedStreams.values().next().value);
}

// ------------------------------------------------------------------ the streaming drawer
//
// makeStreamDrawer draws a reply that is still arriving. It is handed a way to read the text (`getText`)
// rather than the text itself: the stream object stays the only place an accumulated reply lives, so the
// drawer cannot drift out of date with it.
//
// Its DOM is two sibling divs inside the message body, and that split is the entire point. `innerHTML`
// rebuilds whatever it is given, so with a single container there is no such thing as "redraw the new
// part": every frame would re-parse and re-create every block the reply finished writing a minute ago.
// Two containers let the finished part be written once and then never touched again. Both carry the `md`
// class, and every markdown rule in the stylesheet is a descendant rule, so nothing about how a reply
// looks changes; the split only exists while text is moving, and the committed entry replaces the whole
// node with an ordinary single-part body when it arrives.
//
// What each mode hands the parser per frame, for a reply of length n:
//
//   full   n       — the path that shipped before the render gate; n² over a reply
//   tail   n − frozen — usually a few hundred characters; n over a reply
//   plain  0       — one parse when the stream is over
export function makeStreamDrawer(host, getText, { mode = resolveStreamMode(), hz, onPaint, frame } = {}) {
  const frozen = el('div', 'md frozen');
  const live = el('div', 'md live');
  host.append(frozen, live);
  if (mode === 'plain') host.classList.add('plain'); // text mode wants its newlines back (see app.css)
  const d = { mode, host, frozen, live, frozenLen: 0, draws: 0, parsed: 0 };
  d.paint = () => {
    const text = getText();
    d.draws++;
    if (mode === 'plain') {
      d.live.textContent = text; // one text node per frame: no parse, no subtree rebuild
      return;
    }
    if (mode !== 'tail') {
      d.parsed += text.length;
      md.renderTo(d.frozen, text); // everything, every time; `live` stays empty by design
      return;
    }
    // `frozen` only ever grows, and only up to a boundary that a blank line guarantees no construct is
    // crossing (md.lastSafeBoundary). Everything still open — a paragraph, an unclosed bold, a table that
    // may gain a row, a fence that has not closed — stays in `live` and is repainted until it closes.
    const cut = md.lastSafeBoundary(text);
    if (cut > d.frozenLen) {
      d.parsed += cut - d.frozenLen;
      md.renderTo(d.frozen, text.slice(0, cut));
      d.frozenLen = cut;
    }
    const tail = text.slice(d.frozenLen);
    d.parsed += tail.length;
    md.renderTo(d.live, tail);
  };
  // onPaint is handed the gate after every frame it allowed. The one place a reader can see what a
  // ceiling is really buying is the message that is being drawn, so the gate tells the header directly.
  //
  // `frame` is other drawing that wants this frame's budget instead of its own. The reasoning panel and
  // the tool cards used to write on every delta, which put the provider's chunk rate back in charge of the
  // page and cost a separate forced layout each; here they share one gate, so a frame flushes once.
  d.gate = makeRenderGate(() => {
    d.paint();
    if (frame) frame();
    if (onPaint) onPaint(d.gate);
  }, hz);
  d.mark = () => d.gate.mark();
  // finish is `done`. It flushes the gate so nothing stays pending, then draws the whole reply once and
  // empties the live part — so the DOM at the end of a stream is *by construction* what one clean render
  // produces, instead of "however the last boundary happened to fall". That single render per reply is
  // what the old path paid per frame, and the committed entry renders it again anyway.
  d.finish = () => {
    const text = getText();
    d.gate.flush();
    d.draws++;
    d.parsed += text.length;
    md.renderTo(d.frozen, text);
    d.live.innerHTML = '';
    d.frozenLen = text.length;
  };
  // repaint is what a tab becoming visible asks for: the gate drew nothing while it was hidden.
  d.repaint = () => d.gate.flush();
  return d;
}

function streamStart(streamId) {
  if (state.streams.has(streamId)) return state.streams.get(streamId);
  if (finishedStreams.has(streamId)) return null;
  const node = el('div', 'msg assistant stream');
  const head = el('header');
  head.appendChild(el('span', 'who', 'assistant'));
  const tag = el('span', 'eid', 'streaming');
  head.appendChild(tag);
  node.appendChild(head);
  const reasoning = el('details', 'reasoning');
  reasoning.appendChild(el('summary', '', 'reasoning'));
  node.appendChild(reasoning);
  const body = el('div', 'body md');
  node.appendChild(body);
  logEl().appendChild(node);
  const s = { id: streamId, node, body, reasoning, tag, content: '', reasoningText: '', rText: null, rShownLen: 0, tools: new Map(), args: new Map(), argsShown: new Map() };
  // Two channels arrive per delta besides the reply: the reasoning text and a tool call's arguments. Both
  // used to be written straight into the DOM as they landed — an O(n) text replacement plus a forced layout
  // per token. Measured over 13 KB of reasoning in 216 deltas: panel open 21.9 s of wall with a long task
  // on almost every delta, panel closed 2.7 s. So they draw on the reply's frame budget, and only while they
  // are visible: a closed <details> is read by nobody, and the committed entry renders both from the log
  // regardless of what the live node did. (What this does not fix: with the panel open on a 62-entry
  // transcript a single frame costs ~90 ms, so no ceiling can hold it — that is a transcript-size problem.)
  const drawThinking = () => {
    if (!reasoning.open) return;
    if (s.rShownLen === s.reasoningText.length) return;
    if (!s.rText) {
      s.rText = document.createTextNode('');
      reasoning.appendChild(s.rText);
      s.rShownLen = 0; // a fresh node starts empty; opening the panel later draws all of it below
    }
    // Append what arrived since the last frame instead of replacing the whole string. Honest numbers: at
    // 100 KB over 60 frames this is 0.14 ms/frame against 0.34 ms/frame for rewriting the whole string —
    // real, but small next to the transcript term below, and it did not move the wall clock here. It stays
    // because it is strictly less work (proportional to what arrived, not to what is already on screen) and
    // because a thinking model can stream 100 KB the way an ordinary reply streams 1 KB.
    s.rText.appendData(s.reasoningText.slice(s.rShownLen));
    s.rShownLen = s.reasoningText.length;
  };
  const drawToolArgs = () => {
    for (const [key, card] of s.tools) {
      const text = s.args.get(key) || '';
      if (card.el.open && s.argsShown.get(key) !== text) {
        s.argsShown.set(key, text);
        card.setArgs(text); // setArgs pretty-prints: a JSON round-trip, worth not doing 80 times a second
      }
    }
  };
  s.drawThinking = drawThinking;
  s.drawToolArgs = drawToolArgs;
  // A frame is only worth asking for when something on screen could change. This sounds like the same
  // sentence as "ask for a frame", and the difference is the whole cost: a change nobody can see still
  // dirties the tree, and the tree being dirty is what the follow-scroll then pays for. My first draft of
  // this function marked unconditionally and measured 20.7 s over 216 reasoning deltas on a 62-entry
  // session — redrawing nothing, eight times slower than not asking for the frame at all (2.7 s).
  const worthDrawing = () => reasoning.open || [...s.tools.values()].some((card) => card.el.open);
  s.markNews = () => {
    if (worthDrawing()) s.drawer.mark();
  };
  // Opening the panel is a click, not a delta: it draws immediately rather than waiting for a frame, and
  // that is what keeps `?hz=0` usable — the thinking that already arrived is there the moment you ask.
  reasoning.addEventListener('toggle', () => {
    if (reasoning.open) drawThinking();
  });
  // The drawer decides what to redraw; the gate inside it decides how often. `s.content` keeps accumulating
  // either way, so a skipped frame loses no characters and a frozen block costs nothing to repaint.
  s.drawer = makeStreamDrawer(body, () => s.content, {
    frame: () => {
      drawThinking();
      drawToolArgs();
    },
    onPaint: (gate) => {
      const hz = gate.measuredHz;
      tag.textContent = hz ? `streaming · ${hz} Hz` : 'streaming';
    },
  });
  // At 0 on the ladder nothing is drawn until the reply is over, and a bubble that shows nothing needs
  // its reason on screen rather than in a manual: say so before the first frame that never comes.
  if (!s.drawer.gate.hz) tag.textContent = 'streaming · drawn when it ends';
  state.streams.set(streamId, s);
  scrollDown();
  return s;
}

export function streamDelta(d) {
  const s = streamStart(d.streamId);
  if (!s) return;
  if (d.reasoning) {
    // Accumulate, and ask for a frame only if the panel or a tool card is actually open (see markNews).
    // Whether anything is drawn stays the frame painter's call, and the rate stays the gate's.
    s.reasoningText += d.reasoning;
    s.markNews();
  }
  if (d.content) {
    s.content += d.content;
    s.drawer.mark();
  }
  const tc = d.toolCall;
  if (tc) {
    const key = tc.index ?? tc.id ?? s.tools.size;
    if (tc.id || tc.name) {
      s.args.set(key, tc.args || '');
      const card = toolCard(tc.id || `#${key}`, tc.name || 'tool', 'running');
      s.tools.set(key, card);
      s.node.insertBefore(card.el, s.body);
      // Opening a card mid-stream asks for its arguments now, same rule as the reasoning panel.
      card.el.addEventListener('toggle', s.drawToolArgs);
    } else {
      s.args.set(key, (s.args.get(key) || '') + (tc.args || ''));
    }
    // Which tool is running is drawn immediately (above), but its arguments accumulate and follow the
    // frame: an args delta used to mean a pretty-print plus a DOM write per token. And as with the
    // reasoning panel, a collapsed card is not a reader — no frame is requested for it.
    s.markNews();
  }
  // `done` closes the window: whatever is still unpainted gets painted now, and the split disappears into
  // one clean render. Without this the last few tokens of a reply could sit in `s.content` until the
  // committed entry arrived, because "render when the window has passed" is only evaluated when new text
  // arrives, and there is no text after the last one.
  if (d.done) {
    s.drawer.finish();
    streamFinish(s);
  }
  scrollDown();
}

// flushStreams repaints every live stream, and is what a tab coming back to the foreground needs: while
// it was hidden the gate drew nothing (a background tab has no reader), so the text on screen is behind
// the text we have already been sent.
export function flushStreams() {
  for (const s of state.streams.values()) s.drawer.repaint();
}

function streamFinish(s) {
  s.node.classList.remove('stream');
  // The header chip says "streaming", and the moment this runs that is no longer true. It goes rather
  // than being reworded: the committed entry brings its own chips (id, model, usage) when it lands, and a
  // finished message keeping a rate it is no longer drawing at would be a small, permanent lie.
  s.tag?.remove();
  markStreamFinished(s.id);
  if (!s.content && !s.tools.size) s.node.remove();
}

// ------------------------------------------------------------------ tool cards

export function toolCard(callId, label, cls) {
  let node = state.tools.get(callId);
  if (node) return node;
  const card = el('details', 'tool-card');
  const summary = el('summary');
  const lbl = el('span', 'lbl', label);
  const badge = el('span', cls || '', '·');
  summary.append(el('span', 'lbl', 'tool'), lbl, badge);
  const args = el('pre', 'args');
  const result = el('pre', 'result');
  card.append(summary, args, result);
  const api = {
    el: card,
    attached: false,
    setArgs(text) {
      args.textContent = pretty(collapseDataUrls(text).text);
      args.style.display = 'block';
    },
    setResult(text, isError) {
      // Base64 is never read in a <pre>: an image arrives as an attachment (rendered above the
      // tool message) and anything else binary is not worth 250 KB of scrolling.
      result.textContent = collapseDataUrls(text || '(empty)').text;
      result.className = 'result' + (isError ? ' err' : '');
    },
    setLabel(text) {
      lbl.textContent = text;
    },
    setBadge(text, cls2) {
      badge.textContent = text;
      badge.className = cls2 || '';
    },
  };
  state.tools.set(callId, api);
  return api;
}

// ------------------------------------------------------------------ input requests

export function requestCard(ev) {
  if (ev.gate) return gateCard(ev.gate);
  if (ev.elicitation) return elicitCard(ev.elicitation);
  return null;
}

// gateCard asks what to do with a summary that came back too large: accept it, ask again, or keep
// the context as it is.
function gateCard(g) {
  const card = el('div', 'card');
  card.appendChild(el('h4', '', `summary too large — attempt ${g.attempts}`));
  card.appendChild(el('div', '', `${fmtN(g.summarySize)} tokens produced, cap ${fmtN(g.summaryCap)}`));
  const row = el('div', 'row');
  for (const [answer, cls, label] of [
    ['accept', 'primary', 'accept anyway'],
    ['retry', '', 'ask again'],
    ['skip', 'danger', 'keep context (skip)'],
  ]) {
    const b = el('button', cls, label);
    b.onclick = async () => {
      b.disabled = true;
      await call('/gate', { id: g.id, answer });
      card.remove();
      state.requests.delete(g.id);
      emit(EV_REFRESH);
    };
    row.appendChild(b);
  }
  card.appendChild(row);
  if (g.preview) {
    const pre = el('pre');
    pre.textContent = g.preview;
    pre.style.maxHeight = '200px';
    pre.style.overflow = 'auto';
    card.appendChild(pre);
  }
  state.requests.set(g.id, card);
  $('#requests').appendChild(card);
  return card;
}

// elicitCard renders an MCP elicitation form from its JSON schema: enum becomes a select, booleans
// become checkboxes, numbers get their bounds, and nothing else is invented.
function elicitCard(q) {
  const card = el('div', 'card');
  card.appendChild(el('h4', '', `input request · ${q.server}${q.tool ? ' · ' + q.tool : ''}${q.mode ? ' · ' + q.mode : ''}`));
  card.appendChild(el('div', '', q.message || ''));
  let schema = {};
  try {
    schema = typeof q.schema === 'string' ? JSON.parse(q.schema) : q.schema || {};
  } catch {
    schema = {};
  }
  const required = schema.required || [];
  const inputs = {};
  for (const [name, prop] of Object.entries(schema.properties || {})) {
    const label = el('label', '', name + (required.includes(name) ? ' \u2217' : ''));
    let input;
    if (prop.enum) {
      input = el('select');
      for (const v of prop.enum) {
        const o = el('option', '', String(v));
        o.value = String(v);
        input.appendChild(o);
      }
    } else if (prop.type === 'boolean') {
      input = el('input');
      input.type = 'checkbox';
    } else if (prop.type === 'number' || prop.type === 'integer') {
      input = el('input');
      input.type = 'number';
      if (prop.minimum !== undefined) input.min = prop.minimum;
      if (prop.maximum !== undefined) input.max = prop.maximum;
    } else {
      input = el('input');
      input.type = 'text';
      if (prop.minLength) input.minLength = prop.minLength;
      if (prop.pattern) input.pattern = prop.pattern;
    }
    inputs[name] = { input, prop };
    label.appendChild(input);
    card.appendChild(label);
  }
  const row = el('div', 'row');
  const answer = async (behavior) => {
    let content;
    if (behavior === 'accept') {
      content = {};
      for (const [name, { input, prop }] of Object.entries(inputs)) {
        if (prop.type === 'boolean') {
          if (input.checked) content[name] = true;
        } else if (input.value !== '') {
          content[name] = prop.type === 'number' || prop.type === 'integer' ? Number(input.value) : input.value;
        }
      }
    }
    card.remove();
    state.requests.delete(q.id);
    await call('/elicit', { id: q.id, behavior, content });
  };
  for (const [behavior, cls, label] of [
    ['accept', 'primary', 'accept'],
    ['decline', '', 'decline'],
    ['cancel', 'ghost', 'cancel'],
  ]) {
    const b = el('button', cls, label);
    b.onclick = () => answer(behavior);
    row.appendChild(b);
  }
  card.appendChild(row);
  state.requests.set(q.id, card);
  $('#requests').appendChild(card);
  return card;
}
