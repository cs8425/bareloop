// The composer: the textarea, its attachments, and the three buttons that talk to the loop.
//
// On a keyboard, Enter sends, Shift+Enter is a newline, and Enter while a run is live *interrupts* and
// sends instead — the interrupt endpoint writes the message and aborts the current call, so the turn is
// redirected rather than queued behind an answer nobody wants any more.
//
// On glass, Enter is a newline and the round button beside the box sends. See `enterSends` below.
import { $, el } from './util.js';
import { state } from './state.js';
import { call } from './api.js';
import { note } from './ui.js';
import { markPinned, scrollDown } from './scroll.js';

const input = () => $('#input');

// What one Enter keystroke is allowed to do, per browser rather than per session: how you prefer to send
// is not a property of the conversation, and a laptop owner should not have to re-decide it for every
// session they open. `auto` is the default and means "ask this pointer".
export const ENTER_MODES = ['auto', 'on', 'off'];

// coarsePointer asks the pair of questions app.css asks when it decides what a hover is (its
// `@media (hover: none)` block is what makes these hit targets thumb-sized), so the layout and the
// keyboard cannot disagree about the device they are on. The conjunction is deliberately the narrower of
// the two: when a device hovers but reports a coarse pointer, keep the keyboard behaviour people already
// have in their fingers, because a redundant tap on the send button is cheap and taking Enter away from a
// laptop is a surprise nobody asked for.
export const isCoarse = () =>
  typeof matchMedia === 'function' && matchMedia('(hover: none) and (pointer: coarse)').matches;

// resolveEnterMode reads the preference the way resolveHz reads the redraw rate: URL, then the pinned
// setting, then the default. The `?enter=` override is not a toy — it is how the two behaviours can be
// compared on one machine, and how the rule can be checked at all on a laptop that will never report a
// coarse pointer.
export function resolveEnterMode(fallback = 'auto') {
  const pick = (raw) => {
    const v = raw === null || raw === undefined ? '' : String(raw).trim().toLowerCase();
    return ENTER_MODES.includes(v) ? v : null;
  };
  let stored = null;
  try {
    stored = localStorage.getItem('bareloop.enterSends');
  } catch {
    // A profile that refuses localStorage is a normal browser setting, not a reason to stop typing.
  }
  const fromUrl = typeof location === 'object' ? new URLSearchParams(location.search).get('enter') : null;
  return pick(fromUrl) ?? pick(stored) ?? pick(fallback) ?? 'auto';
}

// enterSends answers the only question the keydown handler has. On `auto`, a coarse pointer sends with
// the button and a fine pointer sends with Enter: a newline on glass used to be impossible (no gesture
// for Shift+Enter), while sending is always one tap of a button that is already there and already works
// in every phase. Losing a keystroke on the glass is a soft cost; a message that cannot be multiline is
// a hole nothing here can fill.
export const enterSends = () => {
  const mode = resolveEnterMode();
  return mode === 'on' || (mode === 'auto' && !isCoarse());
};

// describeEnterMode is how the UI admits what it is about to do with a keystroke, in words — a row that
// says "auto" without saying what auto means is how a user ends up convinced the box is broken.
export const describeEnterMode = (mode) => {
  if (mode === 'on') return 'Enter sends';
  if (mode === 'off') return 'Enter adds a line';
  return isCoarse() ? 'this is touch — Enter adds a line, the send button sends' : 'this is a keyboard — Enter sends';
};

// The promise the placeholder makes about the keyboard. index.html ships the keyboard wording as the
// first paint; `placeholderOnSends` captures that copy at wire time so the two strings cannot drift,
// and the box is re-worded whenever the device says Enter means something else. A placeholder telling a
// phone owner to press Shift+Enter is not a hint, it is a lie about a gesture they do not have.
const PLACEHOLDER_NEWLINE = 'message — Enter adds a line, the send button sends, paste or drop images';
let placeholderOnSends = '';

function renderAttachments() {
  const box = $('#attachments');
  box.textContent = '';
  state.attachments.forEach((att, i) => {
    const wrap = el('div', 'att');
    const img = el('img');
    img.src = `data:${att.mimeType};base64,${att.data}`;
    img.alt = att.name;
    const x = el('div', 'x', '×');
    x.title = 'remove this attachment';
    x.onclick = () => {
      state.attachments.splice(i, 1);
      renderAttachments();
    };
    wrap.append(img, x);
    box.appendChild(wrap);
  });
  syncComposer();
}

// syncComposer is the single place that decides what the composer's right-hand slot offers:
// idle -> send (dim until there is a draft), running -> the red square and the bolt.
//
// It runs on every keystroke, on every attachment change and on every status frame, so the glyph is
// never a guess about the server: `draft()` maps base64 blobs and is far too expensive to call here,
// which is why the emptiness test looks at the textarea and the attachment list directly.
export function syncComposer(running) {
  const r = running ?? state.status?.phase === 'running';
  const has = !!input().value.trim() || !!state.attachments.length;
  const send = $('#btn-send');
  const stop = $('#btn-stop');
  const interrupt = $('#btn-interrupt');
  send.classList.toggle('hidden', r);
  stop.classList.toggle('hidden', !r);
  interrupt.classList.toggle('hidden', !r);
  send.disabled = !has || !state.id;
  interrupt.disabled = !has || !state.id;
  // Two things the software keyboard and the placeholder should agree with the handler about, because a
  // label that contradicts the keystroke is worse than no label. The on-screen return key is drawn as
  // the action it will perform, and the hint text stops promising a Shift nobody can press. Both are
  // written only when they change: this function runs on every keystroke, and a redundant attribute write
  // into a focused textarea is a repaint request nobody asked for.
  const sends = enterSends();
  const hint = sends ? 'send' : 'enter';
  if (input().getAttribute('enterkeyhint') !== hint) input().setAttribute('enterkeyhint', hint);
  const ph = sends ? placeholderOnSends : PLACEHOLDER_NEWLINE;
  if (ph && input().placeholder !== ph) input().placeholder = ph;
}

// addFiles reads images to base64: the log stores base64, so an image attached here is replayable
// forever without a sidecar directory. Anything that is not an image is refused out loud, because a
// silently dropped file is a message that never gets sent.
export function addFiles(files) {
  for (const f of files || []) {
    if (!f.type.startsWith('image/')) {
      note(`ignored ${f.name}: only images can be attached`);
      continue;
    }
    const reader = new FileReader();
    reader.onload = () => {
      const dataUrl = String(reader.result);
      state.attachments.push({ name: f.name, mimeType: f.type, data: dataUrl.slice(dataUrl.indexOf(',') + 1) });
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
  markPinned(); // sending a message means "show me the answer", before the server has anything to show
  await call('/send', body);
  scrollDown(true);
}

async function interruptSend() {
  const body = draft();
  if (!body || !state.id) return;
  clearDraft();
  markPinned();
  await call('/interrupt', body);
  scrollDown(true);
}

async function stop() {
  if (!state.id) return;
  await call('/stop', {});
}

export function wireComposer() {
  placeholderOnSends = input().placeholder; // capture the HTML's copy before the first syncComposer rewrites it
  $('#btn-send').onclick = send;
  $('#btn-interrupt').onclick = interruptSend;
  $('#btn-stop').onclick = stop;
  $('#files').onchange = (e) => {
    addFiles(e.target.files);
    e.target.value = '';
  };
  input().addEventListener('input', () => syncComposer());
  syncComposer(false);
  // Dropping an image anywhere in the composer is the same as pasting one; dragover has to be
  // cancelled or the browser navigates to the file instead.
  const composer = $('#composer');
  composer.addEventListener('dragover', (e) => e.preventDefault());
  composer.addEventListener('drop', (e) => {
    e.preventDefault();
    addFiles([...(e.dataTransfer?.files || [])]);
  });
  // A composing Enter is somebody choosing a candidate word, not somebody sending. `isComposing` is the
  // standard signal, but the legacy keyCode 229 still shows up during CJK conversion on engines that
  // refuse to report the real key, and Android keyboards have been seen delivering one bare Enter right
  // after compositionend — so the moment the last conversion ended is remembered as well. Eating an
  // Enter the box then swallows as a newline is recoverable (it is visible, and Backspace is one key);
  // firing a send with half a word in it is not.
  let composedAt = 0;
  input().addEventListener('compositionend', () => {
    composedAt = Date.now();
  });
  input().addEventListener('keydown', (e) => {
    if (e.key !== 'Enter') return; // why: every other key is the textarea's business, not ours.
    if (e.isComposing || e.keyCode === 229) return; // why: the IME is mid-conversion, see above.
    if (e.shiftKey) return; // why: Shift+Enter has always been "a line", on every device.
    if (Date.now() - composedAt < 100) return; // why: the follow-up Enter described above; the window is
    // deliberately tiny — a second Enter that fast was not a decision.
    if (!enterSends()) return; // why: on glass (or when told `off`) Enter means newline, so fall through
    // to the browser and let it insert one; the button beside the box is the send gesture there.
    e.preventDefault();
    if (state.status?.phase === 'running') interruptSend();
    else send();
  });
  input().addEventListener('paste', (e) => {
    const items = [...(e.clipboardData?.items || [])].filter((i) => i.type.startsWith('image/'));
    if (!items.length) return;
    e.preventDefault();
    addFiles(items.map((i) => i.getAsFile()).filter(Boolean));
  });
}
