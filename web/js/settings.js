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
import { resolveHz, describeHz, AUTO, AUTO_INITIAL_HZ } from './scheduler.js';
import { resolveEnterMode, describeEnterMode, syncComposer } from './composer.js';
import { fmtZoneLabel, setTzSetting, tzSetting } from './clock.js';
import { wireMcpEditor } from './mcp-form.js';

// fld reads a control by name: form.elements has properties like "action" and "name" that a plain
// property access would resolve to the wrong thing.
const fld = (form, name) => form.elements.namedItem(name);

// currentConfig fetches the config the server is willing to show (an API key is never sent back).
// The ?config=1 is the whole point: without it this endpoint answers with the entire path — every
// entry, every image re-inflated — and the dialog reads one field out of it.
export async function currentConfig() {
  const data = await get('?config=1');
  return data.config || {};
}

// The MCP editors (form + JSON), built once when the dialog is first wired. `null` until then, which is why
// openSettings asks with `?.`.
let mcp = null;
let pane = 'session';

// showPane switches between the two halves of the dialog, and between the two save buttons with it. Which one
// is on screen is the only thing that decides what Enter in a field means, so the answer is kept here rather
// than read back off a class at submit time.
function showPane(which) {
  pane = which;
  $('#pane-session').classList.toggle('hidden', which !== 'session');
  $('#pane-browser').classList.toggle('hidden', which !== 'browser');
  $('#tab-session').classList.toggle('on', which === 'session');
  $('#tab-browser').classList.toggle('on', which === 'browser');
  $('#settings-save-session').classList.toggle('hidden', which !== 'session');
  $('#settings-save-browser').classList.toggle('hidden', which !== 'browser');
  // The daemon refuses this PATCH while a run holds the request shape open, and the honest UI is the button
  // that says so before it is pressed rather than after. The other half stays live — that is the whole point
  // of splitting them.
  const busy = state.status?.phase === 'running' || state.status?.phase === 'compacting';
  const btn = $('#settings-save-session');
  btn.disabled = busy;
  btn.title = busy
    ? `a run is in flight — the model, the tool set and the MCP list cannot change under a half-sent request. Stop it, or use the other tab`
    : '';
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
  // The server normalises an absent keepBudget to 20 on every read, so this field arrives filled; the ?? 20
  // only covers a config written before the field existed. 0 is a real value (compact-everything tail).
  fld(f, 'keepBudget').value = cfg.compact?.keepBudget ?? 20;
  fld(f, 'summaryMax').value = cfg.compact?.summaryMax ?? 70;
  fld(f, 'maxAttempts').value = cfg.compact?.maxAttempts ?? 3;
  // 0 is what the server runs on when the key is absent (DefaultParams/Normalise), so showing 0 for a session
  // that never set it is not a guess — and saving that 0 back changes nothing.
  fld(f, 'minKeptMessages').value = cfg.compact?.minKeptMessages ?? 0;
  // The two below coerce differently, and that difference is the rule for this whole block: minKeptUserTurns is a
  // floor like the one above it, so 0 is a real answer and only the default of 4 comes from the server, while
  // minSummarizeTokens has no meaningful 0 and an empty box means its default of 2000 — like the percentages.
  fld(f, 'minKeptUserTurns').value = cfg.compact?.minKeptUserTurns ?? 4;
  fld(f, 'minSummarizeTokens').value = cfg.compact?.minSummarizeTokens ?? 2000;
  fld(f, 'maxTurns').value = cfg.maxTurns || '';
  // The redraw rate is not config — it is this browser's preference — but it lives in this dialog
  // because it is a knob people only reach for once and then forget. Show what is actually in force
  // (URL wins over storage), so the row never lies about the current tab.
  const hzNow = resolveHz();
  const autoNow = hzNow === AUTO;
  fld(f, 'renderHzAuto').checked = autoNow;
  // In auto the number box is a reading, not an input: it shows the rung the gate is on right now, and only
  // a live stream can answer that. Nothing streaming means no frame has been timed yet, which is exactly
  // what AUTO_INITIAL_HZ means out loud.
  fld(f, 'renderHz').value = String(autoNow ? liveAutoHz() : hzNow);
  fld(f, 'renderHz').disabled = autoNow;
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
  // The ceiling is a number the server reads 0 as "unset", so an empty field is not a zero: it means
  // "this session never said", and the placeholder carries what is in force instead of the value box.
  fld(f, 'structuredMaxBytes').value = cfg.structuredMaxBytes || '';
  // Prices are three numbers or nothing, and the difference between "0" and "no price at all" has to
  // survive a round trip through a form: an empty field stays empty, a stored zero shows a zero.
  const price = cfg.llm?.pricing;
  fld(f, 'priceInput').value = price ? price.input ?? 0 : '';
  fld(f, 'priceCached').value = price ? price.cachedInput ?? 0 : '';
  fld(f, 'priceOutput').value = price ? price.output ?? 0 : '';
  fld(f, 'systemPrompt').value = cfg.systemPrompt || '';
  // The timezone the time tools answer in, shown as stored. An empty box means "unchanged", which is the
  // PATCH rule for this field and the reason the placeholder says so out loud rather than lying with a
  // plausible-looking "UTC".
  fld(f, 'timezone').value = cfg.timezone || '';
  fld(f, 'mcp').value = JSON.stringify(cfg.mcp || [], null, 2);
  compactMath(f);
  // The MCP editors are reloaded from the textarea the config was just written into, and the dialog opens on
  // the half the button was pressed for: the form, because that is what a person editing a URL wants.
  mcp?.render();
  mcp?.show('form');
  $('#mcp-hint').classList.remove('dirty');
  // The client half is read from storage every time, not from a cache: another tab may have changed it, and
  // this is the one setting the daemon cannot arbitrate.
  fld(f, 'tz').value = tzSetting();
  const now = $('#tz-now');
  if (now) now.textContent = `right now this browser is in ${fmtZoneLabel()} — leave it empty to keep that answer.`;
  showPane('session');
  $('#dlg-settings').showModal();
}

// tok writes a token count the way every token count in this project's own notes is written: digits and a
// space every three. `fmtN` is deliberately not used: it rounds 8 192 to "8k", and a line whose entire job is
// to let someone check their arithmetic against the server's cannot afford to be the one that rounds.
const tok = (n) => String(n).replace(/\B(?=(\d{3})+(?!\d))/g, ' ');

// pct is the server's own integer arithmetic: Params.TriggerLevel, SummaryCap and Budget all compute
// `nctx * pct / 100` on ints, so rounding down here is agreeing with it rather than inventing a style.
const pct = (nctx, p) => Math.trunc((nctx * p) / 100);

// compactMath says out loud, in tokens, what the three percentages of the compaction block add up to — and what
// the kept-tail floor does when they leave no room at all.
//
// They are each other's arithmetic and three columns of labels cannot say so: the trigger is also the ceiling
// afterwards, the summary cap caps the summary ALONE, and the kept tail gets the difference — which is why the
// tail has no field of its own, except for minKeptMessages: that is a floor under the tail, and a floor is what
// the tail is made of when it disagrees with the budget (internal/compact/compact.go: Select, floorCut).
// The formulas and both failure sentences are the server's (TriggerLevel, SummaryCap, Budget, ReasonNCTXUnknown,
// ReasonNoBoundary), re-derived here so the answer is there while the fields are still being edited.
//
// The coercion is saveSettings' own (|| 70, || 50, || 0) — except keepBudget, whose 0 is an answer and
// therefore reads through a trim check instead of ||. An empty box is stored as the default, so the line
// previews what pressing save does instead of the empty field. It is written a second time rather than
// extracted because the PATCH body reads better with each default beside its own field, and the two places
// are ten lines apart.
function compactMath(f) {
  const out = $('#compact-math');
  // why: the markup and this script are two files. If that paragraph is ever renamed or dropped, the dialog
  // still has to open and save — losing its arithmetic line is not a reason to throw on the way in.
  if (!out) return;
  const nctx = Number(fld(f, 'nCtx').value) || 0;
  const trigger = Number(fld(f, 'threshold').value) || 70;
  const cap = Number(fld(f, 'keepTarget').value) || 50;
  // keepBudget reads differently from every number above: its 0 is the compact-everything tail, so an
  // explicit 0 must preview as 0 while an empty box still means "the default the save will store".
  const kbRaw = fld(f, 'keepBudget').value.trim();
  const kb = kbRaw === '' ? 20 : (Number(kbRaw) || 0);
  // minKeptMessages is the floor under the tail, so the line cannot be written without it: with 0 the tail is
  // whatever the budget leaves, with N the tail is at least the newest N raw messages. Same coercion as
  // saveSettings.
  const kept = Number(fld(f, 'minKeptMessages').value) || 0;
  // why: every number here is a share of n_ctx, so without one there is no budget to state. 0 is also the
  // documented "probe the server" answer, so the line says which of the two it is rather than printing a zero
  // budget. This is the one case where nothing runs at all: runCompact refuses on n_ctx <= 0 before it asks
  // whether the call was manual (ReasonNCTXUnknown), so unlike the cap misconfiguration below there is no way
  // to force it.
  if (nctx <= 0) {
    out.textContent = 'n_ctx 0 asks the server for the window: until it answers there is no budget and nothing compacts, manual included';
    return;
  }
  const tail = pct(nctx, kb);
  // why: a tail of 0 tokens is a legal answer, not a broken configuration, and the server makes the same cut for
  // every trigger (compact.Select has no manual argument any more). With nothing left to keep, the cut that fits
  // is "after the last entry" — head + summary and nothing else — unless minKeptMessages floors the tail, in
  // which case the budget gives way and the newest N raw messages stay whatever they cost (spec §2.1). Saying
  // that out loud matters more than the arithmetic, because the summary is capped at cap% while the trigger sits
  // at trigger%: after such a compaction the next request can be over the trigger already, and it fires again.
  // What can still refuse is a floor the browser cannot evaluate — minSummarizeTokens against the size of the
  // region, or a minKeptMessages no legal cut can reach — so the line says where that answer lives instead of
  // pretending to compute it.
  if (tail === 0) {
    out.textContent =
      `kept budget ${kb}% leaves the tail nothing: ` +
      (kept > 0
        ? `min kept messages ${kept} decides the tail — the newest ${kept} raw messages stay whatever they cost (inspect shows the number) and everything older is summarised. `
        : 'a compaction keeps head + summary only. ') +
      `The summary alone may be ${tok(pct(nctx, cap))} tokens against a ${tok(pct(nctx, trigger))}-token trigger, so it can fire again next turn`;
    return;
  }
  out.textContent =
    `fires above ${tok(pct(nctx, trigger))} tokens · the summary must fit ${tok(pct(nctx, cap))} · ` +
    `the tail you keep gets ${tok(tail)} (${kb}% of n_ctx ${tok(nctx)})` +
    // why "gets" becomes "at least": the floor is in messages and the browser has no per-message sizes, so the
    // percentage is a lower bound the moment a floor exists — stating it as exact would be inventing a number.
    (kept > 0 ? `, never fewer than the newest ${kept} raw messages` : '');
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
      // 0 is an answer here too (the compact-everything tail), so unlike the `||` coercions above an empty box
      // falls back to the default while a typed 0 travels — quietly storing 20 would veto the veto.
      keepBudget: fld(f, 'keepBudget').value.trim() === '' ? 20 : Number(fld(f, 'keepBudget').value),
      summaryMax: Number(fld(f, 'summaryMax').value) || 70,
      maxAttempts: Number(fld(f, 'maxAttempts').value) || 3,
      // `|| 0`, not a safer default: an empty box has to land on 0 (empty kept allowed, the server's own
      // default), because quietly storing 1 here would switch the brake on in a session the person never asked
      // to protect.
      minKeptMessages: Number(fld(f, 'minKeptMessages').value) || 0,
      // A floor, so 0 travels: clearing the box means "no floor", and quietly storing the shipped default of 4
      // would put a floor back on a session that just asked for none.
      minKeptUserTurns: Number(fld(f, 'minKeptUserTurns').value) || 0,
      // This one has no 0 to travel (the server reads anything below 1 as the default), so an empty box is the
      // default here as well.
      minSummarizeTokens: Number(fld(f, 'minSummarizeTokens').value) || 2000,
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
    // An empty field goes back as 0, which is what the server reads as "unchanged" — the same
    // compromise the word above it lives with. The one number the server refuses is a negative,
    // because it promises no limit and would be read back as the default; its reason arrives here as
    // the dialog's error rather than as a saved setting.
    structuredMaxBytes: Number(fld(f, 'structuredMaxBytes').value || 0),
    mcp,
    // Timezone rides this PATCH only when the box has something in it. An absent timezone means "unchanged" to
    // the daemon, so this dialog cannot express "back to no opinion" — that is a config.json edit, and the
    // field's hint says so rather than letting an empty box look like a reset.
    ...(fld(f, 'timezone').value.trim() ? { timezone: fld(f, 'timezone').value.trim() } : {}),
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

// liveAutoHz reports the rung auto has settled on. It asks a stream rather than a setting because the rung
// is a property of the machine right now, not of anything stored: with no stream drawing there is nothing
// to read, and the honest answer is the rung auto starts from.
function liveAutoHz() {
  for (const s of state.streams.values()) {
    const a = s.drawer?.gate.auto;
    if (a) return a.target;
  }
  return AUTO_INITIAL_HZ;
}

// applyRenderHz stores the redraw rate and pushes it into any stream already on screen. It deliberately
// does not go into the PATCH body: how fast this laptop can redraw markdown is not a property of the
// session, and writing it there would make one slow machine the session's problem.
function applyRenderHz(f) {
  const auto = fld(f, 'renderHzAuto').checked;
  const raw = auto ? AUTO : fld(f, 'renderHz').value.trim();
  const hz = auto ? AUTO : Number(raw);
  // Why refuse instead of clamp: a box holding `12o` or `-5` means something was typed that this setting
  // cannot mean, and an empty box would become `0` by `Number('')` — a rate of "draw nothing until the
  // reply is over", which is the opposite of what an empty field suggests. Storing a corrected number
  // silently would leave the setting disagreeing with the digits on screen, and the next person to open
  // this dialog would read that lie back as their own choice.
  if (!auto && (raw === '' || !Number.isFinite(hz) || hz < 0)) {
    toast('redraw rate: a number of Hz (0 or more), or tick auto');
    return false;
  }
  try {
    localStorage.setItem('bareloop.renderHz', String(raw));
  } catch {
    // A profile that refuses localStorage still gets the choice for this tab via setHz below.
  }
  for (const s of state.streams.values()) s.drawer?.gate.setHz(hz);
  // A toast, not a note: `note` appends into #log, and the refresh() that saveSettings just ran rebuilds
  // that log — a confirmation eaten by its own save is no confirmation. This one also has to be readable
  // after the dialog closes with the transcript scrolled somewhere else.
  toast(`redrawing at ${describeHz(hz)}${new URLSearchParams(location.search).has('hz') ? ' — the ?hz= in the address bar still wins until reload' : ''}`);
  return true;
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
  const f0 = $('#settings-form');
  // The two MCP editors share the one textarea the config is written into; the form commits into it on every
  // keystroke, the JSON tab commits on blur. The hint is where an edit announces itself as not-saved-yet:
  // nothing reaches the daemon until the session half is saved, and a form this much like a settings panel
  // invites exactly that mistake.
  mcp = wireMcpEditor({
    form: $('#mcp-form'),
    json: fld(f0, 'mcp'),
    addBtn: $('#mcp-add'),
    onEdit: () => $('#mcp-hint').classList.add('dirty'),
  });

  // Two save buttons, because the two halves fail differently: the session half can be refused by the daemon
  // (a run is holding the request shape open) and the browser half cannot be refused at all — it is
  // localStorage. One button for both meant a refused PATCH took the client edits down with it.
  const saveSession = async () => {
    try {
      await saveSettings();
      dlg.close();
    } catch (err) {
      // Bad MCP JSON is the user's own text: it is already on screen, and the dialog stays open so
      // it can be fixed. Anything else (a run in progress, a rejected config) is worth a toast too.
      if (!(err instanceof SyntaxError)) fail(err);
    }
  };
  const saveBrowser = async () => {
    if (!applyRenderHz(f0)) return; // it has already said why in a toast
    applyEnterSends(f0);
    // A zone this browser cannot name is refused rather than stored: a typo would leave every timestamp in
    // the transcript falling back to the browser's own zone while the setting claimed otherwise.
    if (!setTzSetting(fld(f0, 'tz').value.trim())) {
      toast('that is not a timezone id this browser recognises (try Asia/Taipei) — nothing stored');
      return;
    }
    toast(`times shown in ${fmtZoneLabel()}`);
    dlg.close();
    // The stamps and the day separators are drawn from the zone, so the change is only visible once the
    // transcript is painted again — and the sidebar titles carry timestamps too.
    refresh().catch(() => {});
    loadSessions().catch(() => {});
  };

  $('#btn-settings').onclick = openSettings;
  $('#settings-save-session').onclick = saveSession;
  $('#settings-save-browser').onclick = saveBrowser;
  $('#tab-session').onclick = () => showPane('session');
  $('#tab-browser').onclick = () => showPane('browser');
  // Ticking auto changes the row it belongs to: the number box stops being an input and becomes a reading
  // of what auto is on. Wiring only the open path would leave someone who just ticked it unable to read the
  // rung, and unable to type a rate again without closing and reopening the dialog.
  fld($('#settings-form'), 'renderHzAuto').onchange = (e) => {
    const box = fld($('#settings-form'), 'renderHz');
    box.disabled = e.target.checked;
    // Why the reading goes in on the way into auto and not on the way out: leaving the last number there
    // when auto is unticked hands the person back the rate the machine was actually paying.
    if (e.target.checked) box.value = String(liveAutoHz());
  };
  // The arithmetic line has to answer while the fields are being edited, so it hangs off every field that sets
  // it — and off `input` rather than `change`, because "4096" typed one digit at a time is worth reading as
  // 4096 before the field is ever blurred.
  for (const name of ['nCtx', 'threshold', 'keepTarget', 'keepBudget', 'minKeptMessages']) {
    fld($('#settings-form'), name).oninput = () => compactMath($('#settings-form'));
  }
  // One save path per way of leaving the dialog: the button, and Enter inside a field. The form has no action,
  // so without this Enter would be an implicit submission — a page reload that throws the edits away. Which
  // half is on screen is what Enter means; the other half's fields are hidden, and hidden fields are still in
  // the form, which is why the dispatch cannot be left to the browser.
  $('#settings-form').addEventListener('submit', (e) => {
    e.preventDefault();
    (pane === 'browser' ? saveBrowser : saveSession)();
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
