// Drawing the transcript: committed entries, live streams, tool cards, and the two kinds of pending
// input request. Everything here renders from the events the server sends; none of it decides
// anything. Refresh requests go out on the bus so this module does not import the session module.
import md from './md.js';
import { $, el, collapseDataUrls, fmtN, logEl, pretty, whoName } from './util.js';
import { state } from './state.js';
import { call } from './api.js';
import { confirmBox } from './ui.js';
import { scrollDown } from './scroll.js';
import { emit, EV_REFRESH } from './bus.js';

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
  if (e.usage) {
    const u = e.usage;
    const parts = [];
    if (u.prompt) parts.push(`prompt ${fmtN(u.prompt)}`);
    if (u.cache) parts.push(`cached ${fmtN(u.cache)}`);
    if (u.completion) parts.push(`out ${fmtN(u.completion)}`);
    head.appendChild(el('span', 'eid', parts.join(' · ')));
  }
  if (e.ms) head.appendChild(el('span', 'eid', `${e.ms} ms`));
  if (e.finishReason) head.appendChild(el('span', 'eid', e.finishReason));
  if (e.ts) head.appendChild(el('span', 'eid', new Date(e.ts).toLocaleTimeString()));
  const tools = el('span', 'eid');
  tools.style.marginLeft = 'auto';
  const del = el('button', 'ghost', 'delete');
  del.onclick = async () => {
    const yes = await confirmBox(`delete ${e.id}?`, {
      title: 'delete entry',
      detail: 'The entry is marked deleted in the log and its children are re-parented — history around it survives.',
      ok: 'delete',
      danger: true,
    });
    if (!yes) return;
    await call('/entry/delete', { id: e.id });
  };
  const fork = el('button', 'ghost', 'branch here');
  fork.onclick = async () => {
    await call('/tip', { id: e.id });
    emit(EV_REFRESH);
  };
  tools.append(fork, del);
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

function streamStart(streamId) {
  if (state.streams.has(streamId)) return state.streams.get(streamId);
  if (finishedStreams.has(streamId)) return null;
  const node = el('div', 'msg assistant stream');
  const head = el('header');
  head.appendChild(el('span', 'who', 'assistant'));
  head.appendChild(el('span', 'eid', 'streaming'));
  node.appendChild(head);
  const reasoning = el('details', 'reasoning');
  reasoning.appendChild(el('summary', '', 'reasoning'));
  node.appendChild(reasoning);
  const body = el('div', 'body md');
  node.appendChild(body);
  logEl().appendChild(node);
  const s = { id: streamId, node, body, reasoning, content: '', reasoningText: '', rText: null, tools: new Map(), args: new Map() };
  state.streams.set(streamId, s);
  scrollDown();
  return s;
}

export function streamDelta(d) {
  const s = streamStart(d.streamId);
  if (!s) return;
  if (d.reasoning) {
    s.reasoningText += d.reasoning;
    if (!s.rText) {
      s.rText = document.createTextNode('');
      s.reasoning.appendChild(s.rText);
    }
    s.rText.textContent = s.reasoningText;
  }
  if (d.content) {
    s.content += d.content;
    md.renderTo(s.body, s.content);
  }
  const tc = d.toolCall;
  if (tc) {
    const key = tc.index ?? tc.id ?? s.tools.size;
    if (tc.id || tc.name) {
      s.args.set(key, tc.args || '');
      const card = toolCard(tc.id || `#${key}`, tc.name || 'tool', 'running');
      s.tools.set(key, card);
      s.node.insertBefore(card.el, s.body);
    } else {
      s.args.set(key, (s.args.get(key) || '') + (tc.args || ''));
    }
    const card = s.tools.get(key);
    if (card) card.setArgs(s.args.get(key) || '');
  }
  if (d.done) streamFinish(s);
  scrollDown();
}

function streamFinish(s) {
  s.node.classList.remove('stream');
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
