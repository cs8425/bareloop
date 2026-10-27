// The settings dialog and the parts of the config that can be changed while the server runs.
//
// Two rules the server imposes and the dialog has to respect: a PATCH is refused while a run is
// active (the tool set or model may not change under a half-sent request), and a field the body does
// not carry is kept rather than cleared — so the body starts from the config the server just
// reported, not from a template.
import { $ } from './util.js';
import { state } from './state.js';
import { api, get } from './api.js';
import { fail, note } from './ui.js';
import { refresh, loadSessions } from './session.js';

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
  fld(f, 'auto').checked = cfg.compact?.auto === true; // off unless explicitly on
  fld(f, 'inLoop').checked = cfg.compact?.inLoop !== false; // on unless explicitly off
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
    mcp,
  };
  // Fields this dialog does not have are dropped rather than guessed at: absent means "unchanged",
  // while sending a zero here would look like an answer.
  delete body.temperature;
  await api(`/api/session/${state.id}`, { method: 'PATCH', body });
  note('saved (MCP servers reconnect if their list changed)');
  refresh().catch(() => {});
}

export function wireSettings() {
  $('#btn-settings').onclick = openSettings;
  $('#settings-save').onclick = async (e) => {
    e.preventDefault();
    try {
      await saveSettings();
      $('#dlg-settings').close();
    } catch (err) {
      // Bad MCP JSON is the user's own text: it is already on screen, and the dialog stays open so
      // it can be fixed. Anything else (a run in progress, a rejected config) is worth a toast too.
      if (!(err instanceof SyntaxError)) fail(err);
    }
  };
  // Renaming lives in the top bar, so it patches just the name on top of what the server has.
  $('#session-name').addEventListener('change', async (e) => {
    if (!state.id) return;
    const cfg = await currentConfig().catch(() => null);
    if (!cfg) return;
    cfg.name = e.target.value;
    await api(`/api/session/${state.id}`, { method: 'PATCH', body: cfg }).catch(fail);
    loadSessions();
  });
}
