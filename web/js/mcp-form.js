// A form for the MCP server list, next to the JSON it edits.
//
// The textarea stays the truth. This module parses it when the form is opened and writes it back, pretty, on
// every edit — so what a save sends is exactly what the JSON tab would have sent, and a hand-tuned entry that
// this form has never heard of is not silently dropped on the way through. Keys the form does not draw
// (`roots`, anything added by a future build) ride along untouched in `_extra` and are written back in front of
// the keys we do know.
//
// The field list is `mcp.Config` (internal/mcp/client.go) and nothing else — the names are the daemon's, so a
// value typed here is the value config.json gets. Two inversions are worth knowing before editing:
// `disabled` is a switch that is drawn as "off" (a list of servers you want off is a short list), and the
// transport key is `type`, whose absence means stdio.
import { $, el } from './util.js';

// One row of the form. `show` is which half of the fields to draw: a stdio entry has no business showing a
// URL box, and an http entry has no business showing a command box — the daemon refuses the mismatch for a
// transport policy (`agent/validatePolicy`: a caFile, an insecure TLS, a proxy on a server it starts itself)
// and says nothing about the rest.
//
// The order below IS the order the card is drawn in, and it is a reading order: the card's own switch, then
// the transport's half, then how the endpoint is reached (url, headers, CA file, proxy, TLS), then the tool
// policy. A switch row spans every column, so where one lands is not cosmetic — it reads as being *about*
// whatever box sits above it. `disabled` used to be the row under the proxy field, and there it reads as
// "do not use the proxy"; it is now the first row, which is what the summary line (`http · off`) already
// says it is. The TLS switch sits after CA file and proxy for the same reason: its track falls under
// "CA file", the setting it overrides, not under a routing box it has nothing to do with.
const FIELDS = [
  { key: 'disabled', label: 'off — never start or connect this server', kind: 'check' },
  { key: 'name', label: 'name', hint: 'what the tools are named after when two servers collide; the command or URL is used when it is empty', ph: 'mcp-vroot' },
  { key: 'type', label: 'transport', kind: 'select', options: [['', 'stdio — start a command'], ['http', 'http — connect to a URL']], hint: 'empty means stdio, which is what the daemon writes back' },
  { key: 'command', label: 'command', show: 'stdio', ph: 'mcp-server' },
  { key: 'args', label: 'args', kind: 'lines', show: 'stdio', hint: 'one argument per line, exactly as the shell would have received them', list: true },
  { key: 'env', label: 'env', kind: 'lines', show: 'stdio', hint: 'one KEY=value per line — on top of the base environment (PATH, HOME, …), overriding a name it brings; the daemon’s own environment is never inherited beyond the base, envPassthrough, and what is written here' },
  { key: 'envPassthrough', label: 'env passthrough', kind: 'lines', show: 'stdio', ph: 'MY_MCP_TOKEN', hint: 'one variable name per line — its value is read from the daemon’s own environment when the process starts, so the secret itself never enters config.json and never comes back from the API; the alternative to writing it into env with ${ENV}' },
  { key: 'envBase', label: 'inherit a base env (PATH, HOME, …)', kind: 'check', defaultTrue: true, show: 'stdio', hint: 'off starts the process with nothing but the env boxes: no PATH, no HOME — for a server whose environment should hold only what you named' },
  { key: 'dir', label: 'working dir', show: 'stdio' },
  { key: 'url', label: 'url', show: 'http', ph: 'http://127.0.0.1:8200/mcp' },
  { key: 'headers', label: 'headers', kind: 'lines', show: 'http', kv: true, ph: 'Authorization: Bearer …', hint: 'one “Key: value” per line — a map in the daemon’s own keys, so it is sent as {"Authorization":"Bearer …"}' },
  { key: 'caFile', label: 'CA file', show: 'http', hint: 'verify this server against this PEM bundle only' },
  { key: 'proxy', label: 'proxy', show: 'http', ph: 'socks5://127.0.0.1:1080', hint: 'socks5://, socks5h://, http:// or https:// — user:password@host:port if it asks for one; “direct” connects without a proxy at all, ignoring HTTP_PROXY; empty leaves the environment and the session’s own proxy in charge' },
  { key: 'insecureTLS', label: 'accept an unverifiable certificate', kind: 'check', show: 'http', hint: 'https only; the daemon refuses it on a stdio entry' },
  { key: 'toolPrefix', label: 'tool prefix', hint: 'vr turns fs_read into vr_fs_read; empty publishes the tool under its own name' },
  { key: 'disabledTools', label: 'tools off', kind: 'lines', hint: 'one per line; the tool’s own name or a glob like write_*', list: true },
  { key: 'structuredContent', label: 'structuredContent', kind: 'select', options: [['', 'auto (session default)'], ['auto', 'auto'], ['fallback', 'fallback — only when there is no text'], ['always', 'always'], ['drop', 'never send it']] },
];

// The keys with a widget, so anything else in the entry can be recognised as somebody else's business.
const KNOWN = new Set(FIELDS.map((f) => f.key));

// A widget's value to the entry, and back. Empty means "do not write the key" for every one of them: the
// daemon's own encoder omits all of these, and an explicit "" is a different answer from an absent key for
// `structuredContent` (the session default comes back) — so this form omits rather than clearing.

// Where a `K=V` / `Key: value` line splits: whichever separator comes first, because a value is allowed to
// hold the other one (`Cookie: a=b`, `X-Ref: http://x`). The box is written with `: ` and an older paste uses
// `=`, so both have to read the same way.
const sepOf = (line) => {
  const c = line.indexOf(':');
  const e = line.indexOf('=');
  if (c < 0) return e;
  if (e < 0) return c;
  return Math.min(c, e);
};

// The pairs a `kv` field holds, from whichever shape the value arrives in. `headers` is a map[string]string
// and the daemon answers it with an object, while the JSON tab is free-hand and can carry a list of `K=V`
// strings; accepting both is what keeps `0: A=b` — an index wearing a header name — out of the box. That was
// not hypothetical: a stdio `env` (`[]string` in Go, a map to this form) came back out as ["0=A=b"], a valid
// []string the child process then read as a variable called 0.
function pairsOf(value) {
  const lines = Array.isArray(value) ? value.map(String) : Object.entries(value || {}).map(([k, v]) => `${k}: ${v}`);
  const out = [];
  for (const line of lines) {
    const i = sepOf(line);
    if (i > 0) out.push([line.slice(0, i).trim(), line.slice(i + 1).trim()]);
  }
  return out;
}

function readField(f, node) {
  if (f.kind === 'check')
    // A `defaultTrue` switch carries the opposite polarity of an ordinary one: its absence from the entry is
    // "yes", so a checked box writes nothing and only an unchecked one writes an explicit false. (`envBase`
    // is the field that needs this; *bool fields whose absence already means false, like insecureTLS, keep
    // the plain rule.)
    return f.defaultTrue ? (node.checked ? undefined : false) : node.checked || undefined;
  const raw = node.value;
  // `kv` is asked before `lines` and that order is the whole point: a map arrives in the box as lines too, and
  // reading those lines as a list writes ["K=V"] where the daemon wants {"K":"V"} — which it answers with
  // `cannot unmarshal array into Go struct field Config.mcp.0.headers of type map[string]string`.
  if (f.kv) {
    const out = {};
    for (const [k, v] of pairsOf(raw.split('\n'))) out[k] = v;
    return Object.keys(out).length ? out : undefined;
  }
  if (f.kind === 'lines') {
    const parts = raw.split('\n').map((s) => s.trim()).filter(Boolean);
    return parts.length ? parts : undefined;
  }
  const v = raw.trim();
  return v === '' ? undefined : v;
}

function writeField(f, node, value) {
  if (f.kind === 'check') {
    node.checked = f.defaultTrue ? value !== false : !!value;
    return;
  }
  if (f.kind === 'select') {
    const v = value == null ? '' : String(value);
    // A value the choices do not include (`type: "sse"`, a structuredContent word a future build adds) gets an
    // option of its own rather than being silently folded into the first choice — which on commit would have
    // deleted the key and answered "default" for it.
    if (v && ![...node.options].some((o) => o.value === v)) node.appendChild(new Option(`${v} — not in the list above`, v));
    node.value = v;
    return;
  }
  if (f.kv) {
    // One pair per line, in the `Key: value` shape this field's own hint has always promised.
    node.value = pairsOf(value).map(([k, v]) => `${k}: ${v}`).join('\n');
    return;
  }
  if (f.kind === 'lines') {
    node.value = (Array.isArray(value) ? value : []).join('\n');
    return;
  }
  node.value = value == null ? '' : String(value);
}

// entryFrom rebuilds one server object: unknown keys first (in the order they came), then the form's keys,
// each only when it has something to say.
function entryFrom(card, extra) {
  const out = { ...(extra || {}) };
  for (const f of FIELDS) {
    const v = readField(f, card.querySelector(`[data-k="${f.key}"]`));
    if (v === undefined) delete out[f.key];
    else out[f.key] = v;
  }
  return out;
}

export function wireMcpEditor({ form, json, addBtn, onEdit }) {
  let mode = 'form';

  const parse = () => {
    const raw = json.value.trim();
    if (!raw) return [];
    const v = JSON.parse(raw);
    if (!Array.isArray(v)) throw new SyntaxError('the MCP config has to be an array of server objects');
    return v;
  };

  function serverCard(entry, index, total) {
    const card = el('details', 'mcp-server');
    card.open = total < 2; // one server open is a form; five open is a wall
    const sum = el('summary');
    const title = el('span', 'mcp-nm');
    sum.appendChild(title);
    const tools = el('span', 'mcp-kind');
    sum.appendChild(tools);
    const acts = el('span', 'mcp-acts');
    const small = (text, title, fn) => {
      const b = el('button', 'ghost');
      b.type = 'button';
      b.textContent = text;
      b.title = b.ariaLabel = title;
      b.onclick = (e) => {
        e.preventDefault(); // a <summary> swallows clicks; this button must not fold the card it lives in
        e.stopPropagation();
        fn();
      };
      acts.appendChild(b);
    };
    small('↑', 'move this server up — the order is what decides which server wins a tool-name collision', () => move(-1));
    small('↓', 'move this server down', () => move(1));
    small('copy', 'duplicate this server (rename the copy before you save)', () => {
      commit();
      const copy = JSON.parse(JSON.stringify(entryFrom(card, card._extra)));
      copy.name = `${copy.name || 'server'}-2`;
      json.value = stringify([...parse(), copy]);
      render();
      onEdit();
    });
    small('×', 'remove this server from the list', () => {
      commit();
      const all = parse();
      all.splice(index, 1);
      json.value = stringify(all);
      render();
      onEdit();
    });
    sum.appendChild(acts);
    card.appendChild(sum);

    const grid = el('div', 'mcp-grid');
    for (const f of FIELDS) {
      const box = el('label', `mcp-f${f.show ? ` show-${f.show}` : ''}`);
      box.appendChild(el('span', '', f.label));
      let node;
      if (f.kind === 'select') {
        node = el('select');
        for (const [v, label] of f.options) {
          const o = el('option', '', label);
          o.value = v;
          node.appendChild(o);
        }
      } else if (f.kind === 'check') {
        // A boolean row is a switch: `dialog input[type=checkbox]` paints the track and `.mcp-f.check` puts
        // the words after it. There was no branch here, so `disabled` and `insecureTLS` fell through to the
        // text input below and drew an empty box no click could change — the value still survived a
        // round-trip, because `checked` was being set and read as a plain property on that input, which is
        // why the JSON tab and the summary line were right while the form said nothing about either.
        node = el('input');
        node.type = 'checkbox';
        node.setAttribute('role', 'switch'); // every other boolean in this dialog says "switch", and the paint
        // is `input[type=checkbox]`, not the role — a screen reader should not hear "unchecked" for a track.
      } else if (f.kind === 'lines') {
        node = el('textarea');
        node.rows = 2;
        node.spellcheck = false;
        if (f.ph) node.placeholder = f.ph; // an empty KV box reads better with its own shape written in it
      } else {
        node = el('input');
        node.type = 'text';
        node.spellcheck = false;
        if (f.ph) node.placeholder = f.ph;
      }
      node.dataset.k = f.key;
      if (f.hint) node.title = f.hint;
      writeField(f, node, (entry || {})[f.key]);
      // A checkbox answers on `change` and everything else on `input`: a text box that rewrites the JSON one
      // keystroke at a time is what lets the two tabs be open at once without a "apply" button to forget.
      node.addEventListener(f.kind === 'check' ? 'change' : 'input', () => {
        commit();
        label();
        onEdit();
      });
      if (f.kind === 'check') box.classList.add('check');
      box.appendChild(node);
      grid.appendChild(box);
    }
    card.appendChild(grid);
    card._extra = Object.fromEntries(Object.entries(entry || {}).filter(([k]) => !KNOWN.has(k)));
    if (Object.keys(card._extra).length) {
      const kept = el('p', 'hint', `kept as written: ${Object.keys(card._extra).join(', ')} — this form does not edit them`);
      card.appendChild(kept);
    }
    const label = () => {
      const name = card.querySelector('[data-k="name"]').value.trim();
      const type = card.querySelector('[data-k="type"]').value || 'stdio';
      // Which half of the form is meaningful is a property of the card, and the CSS keys on it: a URL box on a
      // stdio entry is not a hint, it is an invitation to write a config the daemon will refuse.
      card.dataset.type = type === 'stdio' ? 'stdio' : 'http';
      title.textContent = name || `server ${index + 1}`;
      title.classList.toggle('muted', !name);
      tools.textContent = card.querySelector('[data-k="disabled"]').checked ? `${type} · off` : type;
    };
    label();
    function move(d) {
      commit();
      const all = parse();
      const j = index + d;
      if (j < 0 || j >= all.length) return;
      [all[index], all[j]] = [all[j], all[index]];
      json.value = stringify(all);
      render();
      onEdit();
    }
    return card;
  }

  // commit writes what the form currently holds back into the textarea, preserving the list's order.
  function commit() {
    const cards = [...form.querySelectorAll('.mcp-server')];
    const all = cards.map((card) => entryFrom(card, card._extra));
    json.value = stringify(all);
  }

  function render() {
    let entries;
    try {
      entries = parse();
      form.classList.remove('bad');
    } catch (err) {
      // The JSON is what the user wrote, so the JSON tab is where the mistake can be seen. Switching
      // automatically beats leaving a form that quietly edited a list it could not read.
      form.textContent = '';
      const box = el('p', 'state-error', `the JSON is not usable yet: ${err.message}`);
      form.appendChild(box);
      show('json');
      return;
    }
    form.textContent = '';
    if (!entries.length) {
      form.appendChild(el('p', 'hint mcp-empty', 'no MCP servers — the model gets only the tools bareloopd answers itself. Add one and it appears in the tool catalogue on the next run.'));
      return;
    }
    entries.forEach((e, i) => form.appendChild(serverCard(e, i, entries.length)));
  }

  const stringify = (v) => JSON.stringify(v, null, 2);

  function show(next) {
    mode = next;
    if (next === 'form') {
      // Form-ward the JSON has to be readable; JSON-ward the form has already committed on every keystroke.
      render();
      $('#mcp-tab-form').classList.add('on');
      $('#mcp-tab-json').classList.remove('on');
      $('#mcp-json').classList.add('hidden');
      form.classList.toggle('hidden', false);
    } else {
      $('#mcp-tab-json').classList.add('on');
      $('#mcp-tab-form').classList.remove('on');
      $('#mcp-json').classList.remove('hidden');
      form.classList.add('hidden');
    }
  }

  $('#mcp-tab-form').onclick = () => show('form');
  $('#mcp-tab-json').onclick = () => show('json');
  addBtn.onclick = () => {
    commit();
    const all = parse();
    all.push({ name: `server-${all.length + 1}`, type: 'http', url: 'http://127.0.0.1:/' });
    json.value = stringify(all);
    render();
    show('form');
    onEdit();
  };
  // The JSON tab is free-hand, so it commits on the way out rather than on every keystroke: half-typed JSON
  // is normal there and a form that redraws under the cursor is not.
  json.addEventListener('change', () => {
    if (mode === 'form') render();
    onEdit();
  });

  return { show, render, commit, mode: () => mode };
}
