// The tool catalogue: which tools a request would carry, what each one costs, what it is for, and
// what it wants as arguments.
//
// Why this is a surface of its own: switching a tool off is a decision about a name you have to
// recognise, and until now the only place that listed names next to their descriptions was the raw
// `tools` array of GET /payload — written for a model, not for a person deciding. So every row here
// leads with the description and the parameter table, and the switch is the first thing on it.
//
// Three facts the server imposes and the drawer has to show honestly:
//   - A policy change lands when the run in flight ends, because the tool block is rendered in front
//     of the history. A flip during a run therefore shows ⏳ instead of pretending to be done.
//   - The policy is matched against the tool's OWN name (`rawName`), while what the model sees can be
//     renamed by `toolPrefix` or by a collision with another server — so a row shows both names.
//   - Switching a whole server off is a config change and is refused mid-run; that switch is locked
//     and says why rather than failing when clicked.
import { $, el, fmtN } from './util.js';
import { state } from './state.js';
import { api, call, get } from './api.js';
import { confirmBox, fail, note } from './ui.js';
import { EV_REFRESH, EV_SESSION, emit, on } from './bus.js';
import { hidePanel, openPanel, panelMode } from './panel.js';

// There is deliberately no prefill-rate constant here. The two numbers an earlier round measured
// (979 tok/s at 687 tokens, 615 tok/s at 1127) moved 10–19 % when the same two servers were measured
// again, and a rate taken at a few hundred tokens is not the rate at 20k: prefill carries a fixed cost
// per request and a rate that keeps rising with the context. So the drawer quotes tokens, which are
// measured, and leaves seconds out until this session's own requests report timings — then the number
// comes from this server at this size, with a sample count beside it.

let view = null; // the last ToolsView: the drawer renders from this and never fetches twice
let filter = '';
let onlyOff = false;
const folded = new Set(); // server names whose tool list is collapsed
let rows = []; // {el, s, t, hay} so the filter hides rows instead of rebuilding the panel
let countEl = null;

// ---------------------------------------------------------------- open and close

// wireTools binds the one handle into the rest of the page: the tool badge in the top bar. It lives
// here rather than in session.js because the badge is a door, not a status readout.
export function wireTools() {
  const badge = $('#tools');
  badge.onclick = toggleToolsDrawer;
  // The badge is a span wearing role=button, and a role without the key handling is a lie: Enter and
  // Space have to do what the click does.
  badge.onkeydown = (e) => {
    if (e.key !== 'Enter' && e.key !== ' ') return;
    e.preventDefault();
    toggleToolsDrawer();
  };
  // A session switch used to leave the previous session's catalogue open under the new session's
  // name — which is a claim about what the next request carries.
  on(EV_SESSION, () => {
    if (panelMode() === 'tools') openTools();
  });
}

export function toggleToolsDrawer() {
  if (panelMode() === 'tools') hidePanel();
  else openTools();
}

export async function openTools() {
  if (!state.id) {
    note('open a session first');
    return;
  }
  const v = await get('/tools').catch(() => null); // api.js already reported it
  if (!v) return;
  view = normalize(v);
  draw();
}

// renderTools is an event handler: the server pushes the whole view whenever the published set
// changes, so a second tab redraws from the frame it was handed instead of polling for it.
export function renderTools(v) {
  if (panelMode() !== 'tools' || !v) return;
  if (v.session && state.id && v.session !== state.id) return;
  view = normalize(v);
  draw();
}

// normalize is not paranoia: a server that is switched off carries no tools at all, and Go's null for
// an empty slice turns every .filter into a TypeError that leaves the panel half drawn.
function normalize(v) {
  v.servers = v.servers || [];
  for (const s of v.servers) s.tools = s.tools || [];
  return v;
}

// syncToolsPanel redraws from the cached view when something else moved the picture: the ⏳ bar and
// the counts come from Status, which arrives on its own events.
export function syncToolsPanel() {
  if (panelMode() === 'tools' && view) draw();
}

// ---------------------------------------------------------------- drawing

function draw() {
  if (!view) return;
  const body = openPanel('tools', `tools · ${view.visible}/${view.total} published`);
  rows = [];
  body.appendChild(summary());
  const waiting = pendingBar();
  if (waiting) body.appendChild(waiting);
  body.appendChild(toolbar());
  for (const s of view.servers || []) body.appendChild(serverBlock(s));
  if (!(view.servers || []).length) body.appendChild(el('div', 'dim', 'this session has no MCP servers — add them in settings'));
  body.appendChild(footnote());
  applyFilter();
}

// The headline numbers: how much of every request is tool definitions, and what changing them costs.
function summary() {
  const box = el('div', 'tsum');
  const onTools = (view.servers || []).flatMap((s) => s.tools.filter((t) => t.enabled));
  const toolTokens = onTools.reduce((n, t) => n + t.estTokens, 0);
  box.appendChild(
    el(
      'div',
      '',
      view.visible
        ? `${view.visible} of ${view.total} tool definitions go into every request — roughly ${fmtN(toolTokens)} tokens of prompt, about ${fmtN(Math.round(toolTokens / view.visible))} each`
        : 'nothing is published to this session right now — a request carries no tool block at all',
    ),
  );
  const cost = el('div', 'cost', costText(toolTokens));
  cost.title = NO_SECONDS;
  box.appendChild(cost);
  return box;
}

// rePrefill is what has to be computed again when the tool block changes: the block sits in front of
// the history, so the history behind it loses its KV prefix. The context figure comes from Status (the
// number the status bar shows) because that is what a request actually cost last time; the one the
// tools view carries is the session's reported usage, which is 0 until this process has seen a reply.
const rePrefill = () => Math.max(0, (state.status?.contextUsed || view?.contextUsed || 0) - publishedTokens());

const publishedTokens = () =>
  (view?.servers || []).flatMap((s) => s.tools.filter((t) => t.enabled)).reduce((n, t) => n + t.estTokens, 0);

// costText quotes tokens only. The one-off cost of a flip is real and it is measurable in tokens —
// turning it into seconds needs this server's prefill rate at this context size, which is what the
// per-message timings will supply; an invented rate would only be wrong by an unknown factor.
function costText(toolTokens) {
  const again = rePrefill();
  if (!toolTokens) return 'with nothing published there is no tool block to send, so there is nothing to save';
  if (!again) return `every request carries these ${fmtN(toolTokens)} tokens; nothing is cached yet, so changing it costs nothing extra`;
  return `every request carries these ${fmtN(toolTokens)} tokens; changing a switch costs one re-prefill of ~${fmtN(again)} cached tokens`;
}

// Why the cost line has no seconds, said once where a curious person will look for it.
const NO_SECONDS =
  'No time estimate on purpose. Prefill is a fixed cost per request plus a rate that rises with context, so a number measured at 700 tokens is not this server’s rate at 20k — the two figures an earlier round measured here moved 10–19 % on a re-run. Tokens are counted, not guessed; seconds come back when this session’s own requests report timings.';

// pendingBar says out loud what the server will not do until the run ends, and offers the only way to
// make it end early.
function pendingBar() {
  if (!(view.pendingChanges || state.status?.toolsPending)) return null;
  const bar = el('div', 'pendbar');
  bar.appendChild(
    el('span', '', `⏳ a tool change is saved but not published: the run in flight still sends ${view.visible} tools, and so does every request already on the wire.`),
  );
  const stop = el('button', '', 'end the run and apply');
  stop.title = 'POST /stop: abort this run, keep what it produced, apply the policy';
  stop.onclick = async () => {
    // call() reports a failure itself, so this only has to say what happened when it worked.
    await call('/stop', {}).then(() => note('run stopped — the tool policy is applied now')).catch(() => {});
    emit(EV_REFRESH);
  };
  bar.appendChild(stop);
  return bar;
}

function toolbar() {
  const bar = el('div', 'tbar');
  const search = el('input', 'tsearch');
  search.type = 'search';
  search.placeholder = 'filter: name, description, parameter — try “grep”';
  search.value = filter;
  search.oninput = () => {
    filter = search.value.trim().toLowerCase();
    applyFilter();
  };
  const off = el('label', 'check');
  const cb = el('input');
  cb.type = 'checkbox';
  cb.checked = onlyOff;
  cb.onchange = () => {
    onlyOff = cb.checked;
    applyFilter();
  };
  off.append(cb, el('span', '', 'only the ones switched off'));
  countEl = el('span', 'dim tcount');
  bar.append(search, off, countEl);
  return bar;
}

function serverBlock(s) {
  const box = el('div', 'srv');
  box.dataset.server = s.name;
  if (folded.has(s.name)) box.classList.add('folded');

  const head = el('div', 'srv-head');
  const fold = el('button', 'fold', folded.has(s.name) ? '▸' : '▾');
  fold.title = 'collapse or expand this server’s tools';
  fold.onclick = () => {
    if (folded.has(s.name)) folded.delete(s.name);
    else folded.add(s.name);
    draw();
  };
  head.appendChild(fold);

  const label = el('div', 'srv-name');
  label.appendChild(el('b', '', s.name));
  const bits = [s.transport];
  if (s.impl) bits.push(s.impl);
  if (s.disabled) bits.push('off: not connected (mcp[].disabled)');
  else if (!s.connected) bits.push('down: not connected');
  if (s.error) bits.push(`error: ${s.error}`);
  label.appendChild(el('div', 'dim', bits.join(' · ')));
  head.appendChild(label);

  head.appendChild(el('span', 'tag', `${s.enabled}/${s.known} on`));
  head.appendChild(quick('all', 'switch every tool of this server on', () => send(s.name, { disabledTools: [] })));
  head.appendChild(quick('none', 'switch every tool of this server off', () => send(s.name, { disabledTools: s.tools.map((t) => t.rawName) })));
  head.appendChild(prefixInput(s));
  head.appendChild(serverSwitch(s));
  box.appendChild(head);
  if (s.note) box.appendChild(el('div', 'srv-note', s.note));
  // Saying it once in plain words, because "off: not connected" next to a checkbox is easy to skim past.
  if (s.disabled) box.appendChild(el('div', 'srv-note', `${s.name} is switched off as a whole — reconnect it with the box on the right to reach any of its tools.`));
  if (!s.disabled && s.prefix && s.prefix !== s.configuredPrefix)
    box.appendChild(el('div', 'srv-note', `the prefix in force is “${s.prefix}”; config says “${s.configuredPrefix}” — it applies when the run ends`));

  const list = el('div', 'srv-tools');
  for (const t of s.tools) list.appendChild(toolRow(s, t));
  if (!s.tools.length)
    list.appendChild(
      el(
        'div',
        'dim',
        s.connected
          ? 'this server publishes no tools'
          : s.disabled
            ? 'nothing listed while the server is off; individual switches stay writable'
            : 'not connected, so nothing is listed yet — the switches still write the policy',
      ),
    );
  box.appendChild(list);
  return box;
}

const quick = (text, title, fn) => {
  const b = el('button', 'mini', text);
  b.title = title;
  b.onclick = fn;
  return b;
};

// The prefix input is deliberately plain and unglamorous: setting it renames every tool this server
// publishes, which is a cache-breaking rename, not a cosmetic option.
function prefixInput(s) {
  const wrap = el('label', 'pfx');
  wrap.title =
    'toolPrefix — publish every tool of this server as "<prefix>_<tool>". Empty (the default) publishes the tool’s own name. Renaming costs the KV prefix of everything already generated, and the old names keep working for this process.';
  wrap.appendChild(el('span', 'dim', 'toolPrefix'));
  const input = el('input');
  input.value = s.configuredPrefix || '';
  input.placeholder = 'none';
  input.size = 8;
  input.spellcheck = false;
  let sent = s.configuredPrefix || '';
  const commit = async () => {
    const next = input.value.trim();
    if (next === sent) return; // Enter commits and then blurs: the second one is a no-op
    sent = next;
    if (await send(s.name, { toolPrefix: next })) s.configuredPrefix = next;
  };
  input.onkeydown = (e) => {
    if (e.key === 'Enter') {
      e.preventDefault();
      commit();
    }
  };
  input.onblur = commit;
  wrap.appendChild(input);
  return wrap;
}

// serverSwitch is the whole-server switch, and it is the one thing in this drawer that is not a tool
// policy: mcp[].disabled changes which servers are connected, so it goes through PATCH, which is
// refused while a run is active.
function serverSwitch(s) {
  const running = state.status?.phase === 'running';
  const lab = el('label', 'check srv-off');
  const cb = el('input');
  cb.type = 'checkbox';
  cb.checked = !s.disabled;
  cb.disabled = running;
  cb.title = running
    ? 'connecting or disconnecting a server is a config change and is refused while a run is active'
    : 'connect this server at all (mcp[].disabled) — its tools cannot be switched individually while it is off';
  cb.onchange = () => setServerDisabled(s, !cb.checked);
  lab.append(cb, el('span', '', 'on'));
  return lab;
}

async function setServerDisabled(s, disabled) {
  // PATCH needs the whole mcp array, and the only read of it is the same heavy GET the settings
  // dialog does — so the config is cached in state.cfg the first time it is needed.
  let cfg = state.cfg;
  if (!cfg) {
    const data = await get('').catch(() => null);
    if (!data) return;
    cfg = state.cfg = data.config || {};
  }
  const mcp = (cfg.mcp || []).map((m) => (m.name === s.name ? { ...m, disabled } : m));
  const ok = await api(`/api/session/${state.id}`, { method: 'PATCH', body: { ...cfg, mcp } })
    .then(() => true)
    .catch((err) => (fail(err), false));
  if (!ok) {
    syncToolsPanel();
    return;
  }
  state.cfg = { ...cfg, mcp };
  note(`${s.name} ${disabled ? 'disconnected: its tools are no longer published' : 'reconnecting'}`);
  emit(EV_REFRESH);
  // The connection changes asynchronously, so give it a moment before reading the catalogue again.
  setTimeout(() => openTools().catch(() => {}), 900);
}

function toolRow(s, t) {
  const row = el('div', 'tool' + (t.enabled ? '' : ' off'));
  row.dataset.key = `${s.name}/${t.rawName}`;

  const cb = el('input');
  cb.type = 'checkbox';
  cb.checked = t.enabled;
  cb.title = t.enabled ? `switch ${t.rawName} off (mcp[].disabledTools)` : `switch ${t.rawName} back on`;
  cb.onchange = () => flip(s.name, t, cb.checked);
  row.appendChild(cb);

  const main = el('div', 'tool-main');
  const name = el('div', 'tname');
  name.appendChild(el('code', '', t.name));
  if (t.renamedFrom) name.appendChild(el('span', 'tag warn', `wanted “${t.renamedFrom}” — another server already had that name, so it is published as “${t.name}”`));
  else if (t.rawName !== t.name) name.appendChild(el('span', 'dim', `policy name: ${t.rawName}`));
  if (t.readOnly) name.appendChild(el('span', 'tag', 'read-only'));
  name.appendChild(
    t.uses
      ? el('span', 'tag ok', `used ${t.uses}× here${t.lastUsed ? ` · last ${t.lastUsed.slice(0, 10)}` : ''}`)
      : el('span', 'tag none', 'never used in this transcript'),
  );
  name.appendChild(el('span', 'tok', `~${fmtN(t.estTokens)} tok/request`));
  main.appendChild(name);
  if (t.title && t.title !== t.name) main.appendChild(el('div', 'ttitle', t.title));
  // A long description is clamped and opened by clicking it: the catalogue is meant to be scanned top
  // to bottom, and one talkative server should not cost forty screens of scrolling.
  const desc = el('div', 'tdesc', t.description || 'no description from the server');
  if ((t.description || '').length > 220 || (t.description || '').includes('\n')) {
    desc.classList.add('clamp');
    desc.title = 'click to read the whole description';
    desc.onclick = () => desc.classList.toggle('open');
  }
  main.appendChild(desc);

  const nParams = Object.keys(t.schema?.properties || {}).length;
  const more = el('button', 'link', `parameters (${nParams}) ▾`);
  let params = null;
  more.onclick = () => {
    if (!params) {
      params = paramsTable(t);
      main.appendChild(params);
    }
    const hidden = params.classList.toggle('hidden');
    more.textContent = `parameters (${nParams}) ${hidden ? '▸' : '▾'}`;
  };
  main.appendChild(more);
  row.appendChild(main);

  rows.push({ el: row, s, t, hay: haystack(s, t) });
  return row;
}

// The parameter table is the reason this drawer exists: whether a tool deserves its place in the
// prompt depends on what it is for and what it wants, and that was only ever readable in the JSON a
// model reads. Anything the table cannot express is said in one line rather than dropped quietly.
function paramsTable(t) {
  const box = el('div', 'params hidden');
  const schema = t.schema || {};
  const props = schema.properties || {};
  const required = new Set(Array.isArray(schema.required) ? schema.required : []);
  const names = Object.keys(props);
  if (!names.length) {
    box.appendChild(el('div', 'dim', schema.additionalProperties === false ? 'takes no arguments' : 'no declared parameters — the schema is open'));
    return box;
  }
  const table = el('table');
  const head = el('tr');
  for (const h of ['parameter', 'type', 'what it is']) head.appendChild(el('th', '', h));
  table.appendChild(head);
  for (const n of names.sort()) {
    const p = props[n] || {};
    const tr = el('tr');
    const cell = el('td', 'tcode', required.has(n) ? `${n} *` : n);
    cell.title = required.has(n) ? 'required' : 'optional';
    tr.appendChild(cell);
    tr.appendChild(el('td', '', typeText(p)));
    const what = el('td', '');
    if (p.description) what.appendChild(el('div', '', p.description));
    if (Array.isArray(p.enum)) what.appendChild(el('div', 'dim', `one of: ${p.enum.join(', ')}`));
    if (p.default !== undefined) what.appendChild(el('div', 'dim', `default: ${JSON.stringify(p.default)}`));
    if (p.items?.description) what.appendChild(el('div', 'dim', `items: ${p.items.description}`));
    tr.appendChild(what);
    table.appendChild(tr);
  }
  box.appendChild(table);
  if (schema.additionalProperties) box.appendChild(el('div', 'dim', '…and accepts properties the schema does not list'));
  box.appendChild(el('div', 'dim', `${names.length} parameter(s), ${required.size} required — this is exactly what the model is told to fill in`));
  return box;
}

function typeText(p) {
  if (Array.isArray(p.anyOf)) return p.anyOf.map((x) => x.type || 'any').join(' | ');
  if (Array.isArray(p.type)) return p.type.join(' | ');
  if (p.type === 'array') return `${p.items?.type || 'any'}[]`;
  if (p.type) return p.type;
  return p.enum ? 'one of' : 'any';
}

// What the filter matches: both names, the description and the parameter names — because what a
// person remembers is "the one that greps", not the exact spelling.
function haystack(s, t) {
  const params = Object.keys(t.schema?.properties || {}).join(' ');
  return `${s.name} ${t.name} ${t.rawName} ${t.renamedFrom || ''} ${t.title || ''} ${t.description || ''} ${params}`.toLowerCase();
}

function applyFilter() {
  const words = filter.split(/\s+/).filter(Boolean);
  const perServer = new Map();
  let shown = 0;
  for (const r of rows) {
    const keep = (!onlyOff || !r.t.enabled) && words.every((w) => r.hay.includes(w));
    r.el.classList.toggle('hidden', !keep);
    if (keep) {
      shown++;
      perServer.set(r.s.name, (perServer.get(r.s.name) || 0) + 1);
    }
  }
  const body = $('#panel-body');
  if (!body) return;
  for (const box of body.querySelectorAll('.srv')) {
    const hits = perServer.get(box.dataset.server) || 0;
    box.classList.toggle('hidden', words.length + (onlyOff ? 1 : 0) > 0 && hits === 0);
  }
  if (countEl) countEl.textContent = shown === rows.length ? `${rows.length} tools` : `${shown}/${rows.length} shown`;
}

// ---------------------------------------------------------------- writing the policy

// flip rewrites the whole disabledTools list of one server, because that is what the API takes and
// what makes a wildcard honest: patterns you typed by hand in settings are replaced by the concrete
// names they covered (the footnote says so — the alternative is a policy nobody can read back).
async function flip(server, tool, turnOn) {
  const s = (view.servers || []).find((x) => x.name === server);
  if (!s) return;
  // Either direction costs the cache: the request carries the tool block in front of the history, so
  // the prefix stops matching as soon as the block is a different array of bytes.
  if (!(await worthWarning(turnOn))) return;
  const disabled = s.tools.filter((t) => (t.rawName === tool.rawName ? !turnOn : !t.enabled)).map((t) => t.rawName);
  await send(server, { disabledTools: disabled });
}

// send is the only write path. The answer is the whole catalogue, so the drawer redraws from it; a
// rejected write (a prefix with a character function calling does not allow) has already been
// reported by api.js, and the redraw snaps the control back to what the server actually has.
async function send(server, patch) {
  const v = await call('/tools', { server, ...patch }).catch(() => null);
  if (!v) {
    syncToolsPanel();
    return false;
  }
  view = normalize(v);
  draw();
  const pending = v.pendingChanges || state.status?.toolsPending;
  note(pending ? 'saved — it takes effect when this run ends' : 'policy applied to this session’s config');
  return true;
}

// worthWarning is about the one cost the user cannot see from the outside: the tool block is sent in
// front of the history, so replacing it means the next request has nothing cached to reuse. Warning
// about every 150-token flip would only train people to click through dialogs, so it asks when what
// has to be computed again is worth more than a few seconds.
async function worthWarning(turningOn) {
  const again = rePrefill();
  if (again < 4000) return true;
  return confirmBox(
    `Re-prefilling about ${fmtN(again)} tokens, because the tool list moves in front of the history. How long that takes belongs to this server at this size — but it is work that was already done.${turningOn ? ' This tool also adds tokens to every request from now on.' : ''}`,
    {
      title: 'the KV cache goes with it',
      detail:
        'Three things this does not do: it does not rewrite history — a tool call already in the transcript is still sent exactly as it was, the model just no longer sees its definition. A prefix rename or an edited description costs the cache just like switching off does. And a tool the model used a lot may send it routing somewhere else instead. The saving is every request; the cost is the next one.',
      ok: 'change it',
    },
  );
}

function footnote() {
  const box = el('div', 'dim tfine');
  box.appendChild(
    el(
      'div',
      '',
      'Switches write mcp[].disabledTools of this server, matched against each tool’s own name, and are applied when the run in flight ends — a request already on the wire cannot change. Renamed and disabled names keep resolving for the rest of this process, so the transcript stays playable. Writing a switch replaces any wildcard you typed in settings with the concrete names it covered.',
    ),
  );
  box.appendChild(el('div', '', 'Token figures are bytes/4 of the tool definition, not a measurement; “used” counts calls on this transcript’s path.'));
  return box;
}
