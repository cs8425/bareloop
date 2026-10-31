// The settings dialog and the parts of the config that can be changed while the server runs.
//
// Two rules the server imposes and the dialog has to respect: a PATCH is refused while a run is
// active (the tool set or model may not change under a half-sent request), and a field the body does
// not carry is kept rather than cleared — so the body starts from the config the server just
// reported, not from a template.
import { $ } from './util.js';
import { state } from './state.js';
import { api, get } from './api.js';
import { fail, note, toast } from './ui.js';
import { refresh, loadSessions } from './session.js';
import { resolveHz, describeHz } from './scheduler.js';
import { resolveEnterMode, describeEnterMode, syncComposer } from './composer.js';

// fld reads a control by name: form.elements has properties like "action" and "name" that a plain
// property access would resolve to the wrong thing.
const fld = (form, name) => form.elements.namedItem(name);

// currentConfig fetches the config the server is willing to show (an API key is never sent back).
export async function currentConfig() {
  const data = await get('');
  return data.config || {};
}

export async function openSettings() {
  if (!state.id) {
    note('open a session first');
    return;
  }
  const cfg = await currentConfig().catch(() => null);
  if (!cfg) return;
  state.cfg = cfg;
  const f = $('#settings-form');
  fld(f, 'name').value = cfg.name || '';
  fld(f, 'model').value = cfg.llm?.model || '';
  fld(f, 'baseUrl').value = cfg.llm?.baseUrl || '';
  fld(f, 'apiKey').value = ''; // there is nothing to show, and empty means "keep the stored key"
  fld(f, 'nCtx').value = cfg.llm?.nCtx || '';
  fld(f, 'threshold').value = cfg.compact?.threshold ?? 70;
  fld(f, 'keepTarget').value = cfg.compact?.keepTarget ?? 50;
  fld(f, 'summaryMax').value = cfg.compact?.summaryMax ?? 70;
  fld(f, 'maxAttempts').value = cfg.compact?.maxAttempts ?? 3;
  fld(f, 'maxTurns').value = cfg.maxTurns || '';
  // The redraw rate is not config — it is this browser's preference — but it lives in this dialog
  // because it is a knob people only reach for once and then forget. Show what is actually in force
  // (URL wins over storage), so the row never lies about the current tab.
  fld(f, 'renderHz').value = String(resolveHz());
  // Same trick as the redraw rate: show what is in force (a ?enter= in the address bar wins), and name
  // the mode the user is reading, not just the word "auto".
  fld(f, 'enterSends').value = resolveEnterMode();
  fld(f, 'auto').checked = cfg.compact?.auto === true; // off unless explicitly on
  fld(f, 'inLoop').checked = cfg.compact?.inLoop !== false; // on unless explicitly off
  // Both assembler switches are on unless the config says otherwise, so the checkboxes start from
  // "not false" rather than from "true".
  fld(f, 'sendReasoning').checked = cfg.asm?.sendReasoning !== false;
  fld(f, 'sendImages').checked = cfg.asm?.sendImages !== false;
  // The structuredContent word is a plain string on the server and absent means "auto", so the picker
  // starts from the word the session is actually running rather than from an empty choice.
  fld(f, 'structuredContent').value = cfg.structuredContent || 'auto';
  // Prices are three numbers or nothing, and the difference between "0" and "no price at all" has to
  // survive a round trip through a form: an empty field stays empty, a stored zero shows a zero.
  const price = cfg.llm?.pricing;
  fld(f, 'priceInput').value = price ? price.input ?? 0 : '';
  fld(f, 'priceCached').value = price ? price.cachedInput ?? 0 : '';
  fld(f, 'priceOutput').value = price ? price.output ?? 0 : '';
  fld(f, 'systemPrompt').value = cfg.systemPrompt || '';
  fld(f, 'mcp').value = JSON.stringify(cfg.mcp || [], null, 2);
  $('#dlg-settings').showModal();
}

export async function saveSettings() {
  const f = $('#settings-form');
  let mcp;
  try {
    mcp = fld(f, 'mcp').value.trim() ? JSON.parse(fld(f, 'mcp').value) : [];
  } catch (err) {
    note(`mcp config is not valid JSON: ${err.message}`, 'error');
    throw err;
  }
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
    // The whole assembler block goes back, checked values and all: `omitToolResultName` is a plain
    // bool on the server, so an absent field there cannot mean "off".
    asm: {
      ...(cfg.asm || {}),
      sendReasoning: fld(f, 'sendReasoning').checked,
      sendImages: fld(f, 'sendImages').checked,
    },
    // Top level, not inside asm: it is a policy about what the model is sent, and asm.* is the block of
    // narrow assembler switches. A select cannot come back empty, which is good — the server reads an
    // empty word here as "keep what is stored", so this row always answers.
    structuredContent: fld(f, 'structuredContent').value,
    mcp,
  };
  // Fields this dialog does not have are dropped rather than guessed at: absent means "unchanged",
  // while sending a zero here would look like an answer.
  delete body.temperature;
  // Prices: all three or none. Half a price would mis-bill every request quietly, and this API has no
  // way back from "priced" to "unpriced" — an absent block means "unchanged", and an explicit null
  // decodes to the same absent — so clearing three fields that held numbers is refused with the reason
  // rather than guessed at. "Free" is a real answer: three zeros, which the panel then bills as 0.
  const priceFields = ['priceInput', 'priceCached', 'priceOutput'];
  const empty = priceFields.filter((n) => fld(f, n).value === '');
  if (empty.length === priceFields.length) {
    if (cfg.llm?.pricing) throw new Error('clearing the price fields does not un-price a session: put 0 in all three to say it is free');
  } else if (empty.length) {
    throw new Error('prices are three numbers or none — fill in input, cached input and output, or leave all three empty');
  } else {
    const nums = priceFields.map((n) => Number(fld(f, n).value));
    if (nums.some((v) => !Number.isFinite(v) || v < 0)) throw new Error('prices have to be numbers, 0 or above');
    body.llm.pricing = { input: nums[0], cachedInput: nums[1], output: nums[2] };
  }
  await api(`/api/session/${state.id}`, { method: 'PATCH', body });
  // The save is only real for this tab once the cache agrees with it: the tools drawer PATCHes
  // {...state.cfg, mcp}, and with a pre-save copy in there it would write the old values back. The
  // refresh below overwrites this with the server's answer (normalised defaults and all).
  state.cfg = body;
  note('saved (MCP servers reconnect if their list changed)');
  refresh().catch(() => {});
}

// applyRenderHz stores the redraw rate and pushes it into any stream already on screen. It deliberately
// does not go into the PATCH body: how fast this laptop can redraw markdown is not a property of the
// session, and writing it there would make one slow machine the session's problem.
function applyRenderHz(f) {
  const raw = fld(f, 'renderHz').value;
  try {
    localStorage.setItem('bareloop.renderHz', raw);
  } catch {
    // A profile that refuses localStorage still gets the choice for this tab via setHz below.
  }
  const hz = Number(raw);
  for (const s of state.streams.values()) s.drawer?.gate.setHz(hz);
  // A toast, not a note: `note` appends into #log, and the refresh() that saveSettings just ran rebuilds
  // that log — a confirmation eaten by its own save is no confirmation. This one also has to be readable
  // after the dialog closes with the transcript scrolled somewhere else.
  toast(`redrawing at ${describeHz(hz)}${new URLSearchParams(location.search).has('hz') ? ' — the ?hz= in the address bar still wins until reload' : ''}`);
}

// applyEnterSends stores how this browser treats the Enter key. Like the redraw rate it is kept out of
// the PATCH body — the way a person types is not a property of the session, and carrying it there would
// make one phone change the behaviour of a session for every device that opens it. The composer reads
// this on the next keystroke, so there is nothing else to push into the page; the toast says what the
// setting resolved to rather than echoing the picker, because `auto` is a description of the device and
// the user is entitled to know which answer the device gave.
function applyEnterSends(f) {
  const raw = fld(f, 'enterSends').value;
  try {
    localStorage.setItem('bareloop.enterSends', raw);
  } catch {
    // A profile that refuses localStorage still gets the choice until the next load.
  }
  const wins = new URLSearchParams(location.search).has('enter');
  // Ask the composer to re-label itself rather than trusting that saveSettings() happened to refresh the
  // transcript: the keystroke and the words above it must change together, and that cannot depend on a
  // side effect of another function. Behaviour was never at risk (the handler re-reads this on every
  // Enter) — only the promise the box makes while it waits for the first keystroke.
  syncComposer();
  toast(`${describeEnterMode(resolveEnterMode())}${wins ? ' — the ?enter= in the address bar still wins until reload' : ''}`);
}

export function wireSettings() {
  const dlg = $('#dlg-settings');
  // One save path for both ways of leaving the dialog: the save button, and Enter inside a field.
  // The form had no submit listener, so Enter was an implicit submission of a form with no action —
  // a page reload that threw the edits away.
  const save = async () => {
    try {
      await saveSettings();
      applyRenderHz($('#settings-form'));
      applyEnterSends($('#settings-form'));
      dlg.close();
    } catch (err) {
      // Bad MCP JSON is the user's own text: it is already on screen, and the dialog stays open so
      // it can be fixed. Anything else (a run in progress, a rejected config) is worth a toast too.
      if (!(err instanceof SyntaxError)) fail(err);
    }
  };
  $('#btn-settings').onclick = openSettings;
  $('#settings-save').onclick = save;
  $('#settings-form').addEventListener('submit', (e) => {
    e.preventDefault();
    save();
  });
  dlg.querySelectorAll('[data-close]').forEach((b) => { b.onclick = () => dlg.close(b.dataset.close || 'cancel'); });
  // Renaming lives in the top bar, so it patches just the name on top of what the server has.
  $('#session-name').addEventListener('change', async (e) => {
    if (!state.id) return;
    const cfg = await currentConfig().catch(() => null);
    if (!cfg) return;
    cfg.name = e.target.value;
    const ok = await api(`/api/session/${state.id}`, { method: 'PATCH', body: cfg })
      .then(() => true)
      .catch((err) => (fail(err), false));
    if (!ok) return;
    state.cfg = cfg; // same mirror rule as saveSettings: this body is now what the server has
    loadSessions();
  });
}
