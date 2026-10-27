// bareloop web UI.
//
// Transport: a websocket carries every agent event (reconnecting automatically); if websockets
// are unavailable the page falls back to server-sent events, and if that fails too a 1.5 s
// polling refresh of the session is used. All actions are plain REST calls, which keeps the
// abort button as close to instant as the network allows.
import md from './md.js';

const $ = (sel) => document.querySelector(sel);
const el = (tag, cls, text) => {
  const node = document.createElement(tag);
  if (cls) node.className = cls;
  if (text !== undefined) node.textContent = text;
  return node;
};
const fmtN = (n) => (n >= 1000 ? (n / 1000).toFixed(n >= 10000 ? 0 : 1) + 'k' : String(n ?? 0));
const clamp = (v, lo, hi) => Math.max(lo, Math.min(hi, v));

const state = {
  sessions: [],
  id: null, // current session id
  entries: new Map(), // entry id -> element
  streams: new Map(), // stream id -> {root, body, reasoning, content, reasoningText, toolEls:Map, toolArgs:Map}
  tools: new Map(), // tool call id -> element
  requests: new Map(), // pending input-request id -> element
  attachments: [], // {name, mimeType, data}
  pending: [],
  status: null,
  seq: 0,
  transport: 'none',
  es: null,
  poll: null,
};

// ---------------------------------------------------------------------------- REST

async function api(path, opts = {}) {
  const res = await fetch(path, {
    headers: { 'content-type': 'application/json' },
    method: opts.method || (opts.body ? 'POST' : 'GET'),
    body: opts.body ? JSON.stringify(opts.body) : undefined,
  });
  const text = await res.text();
  let data = null;
  if (text) {
    try {
      data = JSON.parse(text);
    } catch {
      data = { raw: text };
    }
  }
  if (!res.ok) {
    const msg = (data && data.error) || `${res.status} ${res.statusText}`;
    throw new Error(msg);
  }
  return data;
}

async function call(path, body) {
  try {
    return await api(`/api/session/${state.id}${path}`, { body: body || {} });
  } catch (err) {
    note(err.message, 'error');
    throw err;
  }
}

// get fetches one resource without a body.
async function get(path) {
  try {
    return await api(`/api/session/${state.id}${path}`);
  } catch (err) {
    note(err.message, 'error');
    throw err;
  }
}

// ---------------------------------------------------------------------------- transport

function setConn(on, label, transport) {
  const dot = $('#conn');
  dot.classList.toggle('on', !!on);
  dot.classList.toggle('off', !on);
  $('#conn-label').textContent = label;
  state.transport = transport || 'none';
}

let wsAttempts = 0;
let ws = null;

function connect() {
  if (state.transport === 'sse' || state.transport === 'poll') return;
  const proto = location.protocol === 'https:' ? 'wss:' : 'ws:';
  try {
    ws = new WebSocket(`${proto}//${location.host}/ws`);
  } catch (err) {
    fallback();
    return;
  }
  ws.onopen = () => {
    wsAttempts = 0;
    setConn(true, 'websocket', 'ws');
    ws.send(JSON.stringify({ type: 'subscribe', session: '' }));
    ws.send(JSON.stringify({ type: 'snapshot', session: state.id || '' }));
  };
  ws.onmessage = (ev) => {
    let msg;
    try {
      msg = JSON.parse(ev.data);
    } catch {
      return;
    }
    if (msg.type === 'reply' || msg.type === 'reply_error') return; // actions use REST
    handleEvent(msg);
  };
  ws.onclose = () => {
    setConn(false, 'reconnecting…', state.transport);
    wsAttempts++;
    if (wsAttempts >= 3) return fallback();
    setTimeout(connect, Math.min(800 * wsAttempts, 4000));
  };
  ws.onerror = () => {
    try {
      ws.close();
    } catch {
      /* ignore */
    }
  };
}

function fallback() {
  if (state.transport === 'sse' || state.transport === 'poll') return startPolling();
  startSSE();
}

function startSSE() {
  if (!window.EventSource || !state.id) return startPolling();
  if (state.es) return;
  const src = new EventSource(`/api/session/${state.id}/events`);
  state.es = src;
  src.onopen = () => {
    setConn(true, 'sse', 'sse');
    refresh().catch(() => {});
  };
  src.addEventListener('agent', (ev) => {
    try {
      handleEvent(JSON.parse(ev.data));
    } catch {
      /* ignore */
    }
  });
  src.onerror = () => {
    if (src.readyState === EventSource.CLOSED) startPolling();
  };
}

function startPolling() {
  setConn(true, 'polling', 'poll');
  if (state.poll) return;
  state.poll = setInterval(() => {
    if (state.id) refresh().catch(() => {});
  }, 1500);
}

// ---------------------------------------------------------------------------- rendering

const log = () => $('#log');

function note(text, kind = 'note') {
  const node = el('div', kind === 'error' ? 'msg error' : 'note', text);
  log().appendChild(node);
  scrollDown();
  if (kind === 'note') setTimeout(() => node.remove(), 9000);
}

function scrollDown() {
  const box = log();
  if (box.scrollHeight - box.scrollTop - box.clientHeight < 220) box.scrollTop = box.scrollHeight;
}

const whoName = (e) => (e.kind === 'compaction' ? 'compaction' : e.role || e.kind);

function entryNode(e) {
  let node = state.entries.get(e.id);
  if (node) return node;
  node = el('div', `msg ${e.kind}${e.interrupted ? ' interrupted' : ''}`);
  const head = el('header');
  head.appendChild(el('span', 'who', whoName(e)));
  head.appendChild(el('span', 'eid', e.id));
  if (e.model) head.appendChild(el('span', 'eid', e.model));
  if (e.tool) head.appendChild(el('span', 'eid', `${e.server ? e.server + '/' : ''}${e.tool}`));
  if (e.isError) head.appendChild(el('span', 'state-error', 'error'));
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
    if (!confirm(`delete ${e.id}? children are re-parented`)) return;
    await call('/entry/delete', { id: e.id });
  };
  const fork = el('button', 'ghost', 'branch here');
  fork.onclick = async () => {
    await call('/tip', { id: e.id });
    refresh();
  };
  tools.append(fork, del);
  head.appendChild(tools);
  node.appendChild(head);

  if (e.reasoning) node.appendChild(reasoningBlock(e.reasoning));

  const body = el('div', 'body' + (e.kind === 'assistant' ? ' md' : ''));
  fillBody(node, body, e);
  node.appendChild(body);

  state.entries.set(e.id, node);
  log().appendChild(node);
  return node;
}

function fillBody(node, body, e) {
  body.textContent = '';
  for (const p of e.parts || []) {
    if (p.type === 'image' && p.data) {
      const img = el('img');
      img.className = 'attach';
      img.src = `data:${p.mimeType || 'image/png'};base64,${p.data}`;
      img.alt = p.name || 'image';
      body.appendChild(img);
    }
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
    pre.textContent = e.content;
    body.appendChild(pre);
  } else {
    body.appendChild(document.createTextNode(e.content));
  }
}

function reasoningBlock(text) {
  const d = el('details', 'reasoning');
  const s = el('summary');
  s.textContent = 'reasoning';
  d.appendChild(s);
  d.appendChild(document.createTextNode(text));
  return d;
}

// streaming ------------------------------------------------------------

function streamStart(streamId) {
  if (state.streams.has(streamId)) return state.streams.get(streamId);
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
  log().appendChild(node);
  const s = { id: streamId, node, body, reasoning, content: '', reasoningText: '', rText: null, tools: new Map(), args: new Map(), entryId: null };
  state.streams.set(streamId, s);
  scrollDown();
  return s;
}

function streamDelta(d) {
  const s = streamStart(d.streamId);
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
  if (!s.content && !s.tools.size) s.node.remove();
}

// tool cards ------------------------------------------------------------

function toolCard(callId, label, cls) {
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
      args.textContent = pretty(text);
      args.style.display = 'block';
    },
    setResult(text, isError) {
      result.textContent = text || '(empty)';
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

const pretty = (text) => {
  if (!text) return '';
  try {
    return JSON.stringify(JSON.parse(text), null, 2);
  } catch {
    return text;
  }
};

// pending input requests (gate / MCP elicitation) ----------------------------

function requestCard(ev) {
  if (ev.gate) return gateCard(ev.gate);
  if (ev.elicitation) return elicitCard(ev.elicitation);
  return null;
}

function gateCard(g) {
  const card = el('div', 'card');
  card.appendChild(el('h4', '', `summary too large — attempt ${g.attempts}`));
  card.appendChild(el('div', '', `${fmtN(g.summarySize)} tokens produced, cap ${fmtN(g.summaryCap)}`));
  const row = el('div', 'row');
  const buttons = [
    ['accept', 'primary', 'accept anyway'],
    ['retry', '', 'ask again'],
    ['skip', 'danger', 'keep context (skip)'],
  ];
  for (const [answer, cls, label] of buttons) {
    const b = el('button', cls, label);
    b.onclick = async () => {
      b.disabled = true;
      await call('/gate', { id: g.id, answer });
      card.remove();
      state.requests.delete(g.id);
      refresh().catch(() => {});
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

// events ------------------------------------------------------------

function handleEvent(ev) {
  if (ev.seq !== undefined) state.seq = Math.max(state.seq, ev.seq || 0);
  if (ev.session && state.id && ev.session !== state.id) {
    // A different session produced this: the sidebar is the only thing that cares.
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
      note(ev.data?.line || '');
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
    // The committed entry supersedes the live stream buffer: move the streamed tool cards into
    // the committed message and drop the buffer.
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
  if (e.kind === 'compaction') refresh().catch(() => {});
  scrollDown();
}

function onToolProgress(d, isResult) {
  if (!d) return;
  const card = toolCard(d.callId, `${d.server ? d.server + '/' : ''}${d.tool}`, d.status === 'error' ? 'state-error' : 'state-running');
  if (d.arguments) card.setArgs(d.arguments);
  if (d.status === 'running') card.setBadge('running', 'state-running');
  if (!isResult) {
    if (!card.attached) {
      const target = state.entries.get(d.entryId);
      (target || log()).appendChild(card.el);
      card.attached = true;
    }
    return;
  }
  const images = d.images ? ` \u00b7 ${d.images} image(s) in the tool message` : '';
  card.setResult((d.error || d.result || '') + images, d.status === 'error');
  if (d.ms) card.setBadge(`${d.ms} ms`, d.status === 'error' ? 'state-error' : 'state-ok');
  if (!card.attached) {
    const target = state.entries.get(d.entryId);
    (target || log()).appendChild(card.el);
    card.attached = true;
  }
}

function onCompaction(r) {
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
  log().appendChild(box);
  if (r.committed) refresh().catch(() => {});
  scrollDown();
}

function renderStatus(st) {
  if (!st) return;
  const phase = $('#phase');
  phase.textContent = st.phase;
  phase.className = 'badge ' + st.phase;
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
  const pct = nctx > 0 ? clamp((100 * used) / nctx, 0, 100) : 0;
  $('#ctx-fill').style.width = pct.toFixed(1) + '%';
  $('#ctx-text').textContent =
    nctx > 0
      ? `${fmtN(used)} / ${fmtN(nctx)} (${pct.toFixed(0)}%, trigger ${st.thresholdPct ?? '-'}%)${st.summary ? ' · summary' : ''}${st.boundaryBroken ? ' · boundary missing' : ''}`
      : `${fmtN(used)} tokens · n_ctx unknown`;
  $('#session-name').value = st.name || st.session || '';
  if (st.pending !== undefined) {
    state.pending = state.pending || [];
  }
}

function renderPending() {
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
  for (const id of state.pending) {
    list.appendChild(el('li', '', id));
  }
}

// ---------------------------------------------------------------------------- session data

// pathOf walks parents from the tip, returning the set of entry ids that are actually sent.
function pathOf(entries, tip) {
  const byId = new Map(entries.map((e) => [e.id, e]));
  const set = new Set();
  let cur = tip;
  while (cur && byId.has(cur)) {
    set.add(cur);
    cur = byId.get(cur).parentId;
  }
  return set;
}

async function refresh() {
  if (!state.id) return;
  const data = await get('');
  log().textContent = '';
  state.entries.clear();
  state.tools.clear();
  for (const [, s] of state.streams) s.node.remove();
  state.streams.clear();
  const entries = data.entries || [];
  const onPath = pathOf(entries, data.status?.tip);
  for (const e of entries) {
    const node = entryNode(e);
    if (!onPath.has(e.id)) {
      node.style.opacity = 0.45;
      node.title = 'not on the current path (branch)';
    }
  }
  state.status = data.status;
  renderStatus(data.status);
  state.pending = data.pending || [];
  renderPending();
  scrollDown();
}

async function loadSessions() {
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
    rm.style.marginLeft = 'auto';
    rm.onclick = async (e) => {
      e.stopPropagation();
      if (!confirm(`delete session ${s.id} and its history?`)) return;
      await api(`/api/session/${s.id}`, { method: 'DELETE' }).catch((err) => note(err.message, 'error'));
      if (state.id === s.id) {
        state.id = null;
        log().textContent = '';
      }
      loadSessions();
    };
    li.appendChild(rm);
    list.appendChild(li);
  }
}

async function selectSession(id) {
  state.id = id;
  log().textContent = '';
  state.entries.clear();
  state.tools.clear();
  state.streams.clear();
  for (const [, card] of state.requests) card.remove();
  state.requests.clear();
  await loadSessions();
  await refresh();
}

// ---------------------------------------------------------------------------- composer

const input = () => $('#input');

function renderAttachments() {
  const box = $('#attachments');
  box.textContent = '';
  state.attachments.forEach((att, i) => {
    const wrap = el('div', 'att');
    const img = el('img');
    img.src = `data:${att.mimeType};base64,${att.data}`;
    const x = el('div', 'x', '×');
    x.onclick = () => {
      state.attachments.splice(i, 1);
      renderAttachments();
    };
    wrap.append(img, x);
    box.appendChild(wrap);
  });
}

function addFiles(files) {
  for (const f of files || []) {
    if (!f.type.startsWith('image/')) {
      note(`ignored ${f.name}: only images can be attached`);
      continue;
    }
    const reader = new FileReader();
    reader.onload = () => {
      const dataUrl = String(reader.result);
      const b64 = dataUrl.slice(dataUrl.indexOf(',') + 1);
      state.attachments.push({ name: f.name, mimeType: f.type, data: b64 });
      renderAttachments();
    };
    reader.readAsDataURL(f);
  }
}

function draft() {
  const text = input().value.trim();
  const images = state.attachments.map((a) => ({ type: 'image', mimeType: a.mimeType, data: a.data, name: a.name }));
  if (!text && !images.length) return null;
  return { text, images };
}

function clearDraft() {
  input().value = '';
  state.attachments = [];
  renderAttachments();
}

async function send() {
  const body = draft();
  if (!body || !state.id) return;
  clearDraft();
  await call('/send', body);
}

async function interruptSend() {
  const body = draft();
  if (!body || !state.id) return;
  clearDraft();
  await call('/interrupt', body);
}

async function stop() {
  if (!state.id) return;
  await call('/stop', {});
}

// ---------------------------------------------------------------------------- settings + panel

// fld reads a control by name (avoids colliding with collection properties).
const fld = (form, name) => form.elements.namedItem(name);

// currentConfig fetches the session config the server is willing to show (the key is stripped).
async function currentConfig() {
  const data = await get('');
  return data.config || {};
}

async function openSettings() {
  if (!state.id) {
    note('open a session first');
    return;
  }
  const cfg = await currentConfig();
  state.cfg = cfg;
  const f = $('#settings-form');
  fld(f, 'name').value = cfg.name || '';
  fld(f, 'model').value = cfg.llm?.model || '';
  fld(f, 'baseUrl').value = cfg.llm?.baseUrl || '';
  fld(f, 'apiKey').value = '';
  fld(f, 'nCtx').value = cfg.llm?.nCtx || '';
  fld(f, 'threshold').value = cfg.compact?.threshold ?? 70;
  fld(f, 'keepTarget').value = cfg.compact?.keepTarget ?? 50;
  fld(f, 'summaryMax').value = cfg.compact?.summaryMax ?? 70;
  fld(f, 'maxAttempts').value = cfg.compact?.maxAttempts ?? 3;
  fld(f, 'maxTurns').value = cfg.maxTurns || '';
  fld(f, 'auto').checked = cfg.compact?.auto === true; // T1: off unless set
  fld(f, 'inLoop').checked = cfg.compact?.inLoop !== false; // T2: on unless set false
  fld(f, 'systemPrompt').value = cfg.systemPrompt || '';
  fld(f, 'mcp').value = JSON.stringify(cfg.mcp || [], null, 2);
  $('#dlg-settings').showModal();
}

async function saveSettings() {
  const f = $('#settings-form');
  let mcp;
  try {
    mcp = fld(f, 'mcp').value.trim() ? JSON.parse(fld(f, 'mcp').value) : [];
  } catch (err) {
    note(`mcp config is not valid JSON: ${err.message}`, 'error');
    return;
  }
  // Start from the config the server has, so nothing that is not in this dialog is lost.
  const cfg = state.cfg || {};
  const body = {
    ...cfg,
    name: fld(f, 'name').value,
    systemPrompt: fld(f, 'systemPrompt').value,
    maxTurns: Number(fld(f, 'maxTurns').value) || 0,
    llm: {
      ...(cfg.llm || {}),
      model: fld(f, 'model').value,
      baseUrl: fld(f, 'baseUrl').value,
      apiKey: fld(f, 'apiKey').value,
      nCtx: Number(fld(f, 'nCtx').value) || 0,
    },
    compact: {
      ...(cfg.compact || {}),
      auto: fld(f, 'auto').checked,
      inLoop: fld(f, 'inLoop').checked,
      threshold: Number(fld(f, 'threshold').value) || 70,
      keepTarget: Number(fld(f, 'keepTarget').value) || 50,
      summaryMax: Number(fld(f, 'summaryMax').value) || 70,
      maxAttempts: Number(fld(f, 'maxAttempts').value) || 3,
    },
    mcp,
  };
  delete body.temperature; // keep the server's value unless the dialog grows a field for it
  await api(`/api/session/${state.id}`, { method: 'PATCH', body });
  note('saved (MCP servers reconnect if their list changed)');
  refresh().catch(() => {});
}

async function showInspect() {
  if (!state.id) {
    note('open a session first');
    return;
  }
  const data = await get('/inspect');
  $('#panel').classList.remove('hidden');
  $('#app').classList.add('with-panel');
  const rows = data.messages || [];
  $('#panel-title').textContent = `inspect · ${rows.length} messages`;
  const body = $('#panel-body');
  body.textContent = '';
  const head = el('div');
  head.appendChild(
    el(
      'div',
      '',
      `${fmtN(data.contextUsed)} of ${fmtN(data.nCtx)} tokens${data.estimated ? ' (estimated: the provider gives no per-request usage)' : ''}`,
    ),
  );
  head.appendChild(
    el(
      'div',
      '',
      `budget ${fmtN(data.budget)} · summary cap ${fmtN(data.summaryCap)} · ${data.systemBytes} bytes of system prompt · ${data.totalBytes} bytes total${data.syntheticToolResults ? ` · ${data.syntheticToolResults} synthetic tool result(s)` : ''}`,
    ),
  );
  if (data.boundaryId) head.appendChild(el('div', '', `boundary ${data.boundaryId}${data.boundaryBroken ? ' (missing!)' : ''}`));
  if (data.planReason) head.appendChild(el('div', '', `next compaction plan: ${data.planReason} (cut ${data.plan?.cut ?? 0})`));
  if (data.tools?.length) head.appendChild(el('div', '', `tools: ${data.tools.join(', ')}`));
  body.appendChild(head);

  const table = el('table');
  const tr = el('tr');
  for (const h of ['id', 'role', 'tokens', 'head']) tr.appendChild(el('th', '', h));
  table.appendChild(tr);
  for (const r of rows) {
    const row = el('tr');
    row.append(el('td', '', r.id || '(synthetic)'), el('td', '', r.role), el('td', 'num', fmtN(r.tokens)), el('td', 'head', r.head || ''));
    if (!r.inKept) row.style.opacity = 0.45;
    if (!r.real) row.style.fontStyle = 'italic';
    table.appendChild(row);
  }
  body.appendChild(table);
  const note2 = el('div', '', 'dimmed rows are summarised away; italic tokens are byte/4 estimates');
  note2.style.marginTop = '6px';
  body.appendChild(note2);
}

// ---------------------------------------------------------------------------- wiring

function wire() {
  $('#btn-send').onclick = send;
  $('#btn-interrupt').onclick = interruptSend;
  $('#btn-stop').onclick = stop;
  $('#btn-compact').onclick = async () => {
    if (!state.id) {
      note('open a session first');
      return;
    }
    const instruction = prompt('extra instruction for the summary (optional)', '') || '';
    note('compacting…');
    const res = await call('/compact', { instruction }).catch(() => null);
    if (res) onCompaction(res);
  };
  $('#btn-inspect').onclick = showInspect;
  $('#btn-settings').onclick = openSettings;
  $('#panel-close').onclick = () => {
    $('#panel').classList.add('hidden');
    $('#app').classList.remove('with-panel');
  };
  $('#files').onchange = (e) => {
    addFiles(e.target.files);
    e.target.value = '';
  };
  input().addEventListener('keydown', (e) => {
    if (e.key === 'Enter' && !e.shiftKey && !e.isComposing) {
      e.preventDefault();
      if (state.status?.phase === 'running') interruptSend();
      else send();
    }
  });
  input().addEventListener('paste', (e) => {
    const items = [...(e.clipboardData?.items || [])].filter((i) => i.type.startsWith('image/'));
    if (!items.length) return;
    e.preventDefault();
    addFiles(items.map((i) => i.getAsFile()).filter(Boolean));
  });
  $('#session-name').addEventListener('change', async (e) => {
    if (!state.id) return;
    const cfg = await currentConfig();
    cfg.name = e.target.value;
    await api(`/api/session/${state.id}`, { method: 'PATCH', body: cfg }).catch((err) => note(err.message, 'error'));
    loadSessions();
  });
  $('#new-session').onclick = () => $('#dlg-new').showModal();
  $('#new-form').addEventListener('submit', async (e) => {
    e.preventDefault();
    const f = e.target;
    const body = {
      id: fld(f, 'id').value.trim(),
      name: fld(f, 'name').value.trim(),
    };
    const data = await api('/api/sessions', { body }).catch((err) => {
      note(err.message, 'error');
      return null;
    });
    f.reset();
    $('#dlg-new').close();
    if (data?.id) await selectSession(data.id);
  });
  $('#settings-save').onclick = async (e) => {
    e.preventDefault();
    await saveSettings().catch((err) => note(err.message, 'error'));
    $('#dlg-settings').close();
  };
  window.addEventListener('beforeunload', () => state.es?.close());
}

async function main() {
  wire();
  await loadSessions();
  const initial = state.sessions.find((s) => !s.closed)?.id;
  if (initial) await selectSession(initial);
  else note('no session yet — create one');
  connect();
}

main();


