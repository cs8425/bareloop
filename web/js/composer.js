// The composer: the textarea, its attachments, and the three buttons that talk to the loop.
//
// On a keyboard, Enter sends and Shift+Enter is a newline — the same keystroke whether a run is live or
// not, because the endpoint already knows the difference: a message sent mid-run is appended to the path and
// injected at the next turn boundary, so Enter keeps meaning "say this" and never quietly aborts work in
// progress. Aborting is a separate gesture with a separate button: the bolt is the only way to reach
// /interrupt, which is what stops generation and starts the loop again on what you just typed.
//
// On glass, Enter is a newline and the round button beside the box sends. See `enterSends` below.
import { $, el } from './util.js';
import { isParked, state } from './state.js';
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
// idle -> send (dim until there is a draft), running -> the red square plus the bolt once a draft exists,
// compacting -> the square alone. A manual summary is not a generation, so it does not get the bolt, whose promise is
// "abort this generation and send this message instead": a summary has written nothing to keep and no answer
// is owed to anything. Typing still works, and what is typed is not lost - agent.holdMessageLocked keeps it off
// the path until the boundary the summary is writing exists, then it lands behind that boundary and earns its
// own run - but an interrupt sent meanwhile is refused (agent.Interrupt), so hiding the bolt keeps the promise
// instead of spending a message on an error. Stop is the only hand on a summary (agent.CompactNow), which is
// why the square appears for it at all.
//
// It runs on every keystroke, on every attachment change and on every status frame, so the glyph is
// never a guess about the server: `draft()` maps base64 blobs and is far too expensive to call here,
// which is why the emptiness test looks at the textarea and the attachment list directly.
export function syncComposer(running, compacting) {
  const r = running ?? state.status?.phase === 'running';
  const c = compacting ?? state.status?.phase === 'compacting';
  const has = !!input().value.trim() || !!state.attachments.length;
  const send = $('#btn-send');
  const stop = $('#btn-stop');
  const interrupt = $('#btn-interrupt');
  send.classList.toggle('hidden', r);
  stop.classList.toggle('hidden', !(r || c));
  // The square ends whatever is in flight, and what is in flight is what the button is promising: a run
  // has half an answer worth keeping, a summary has written nothing yet. The words are set here because
  // this function is the only thing that knows which of the two it is showing.
  stop.title = c && !r ? 'stop this summary - nothing is written until it finishes' : 'stop the run';
  // The bolt appears only once there is a draft to send with it. Its promise is "abort this generation
  // and say this instead", and with nothing typed that promise is empty; worse, a bolt standing permanently
  // above the stop square is a second round target stacked under a thumb, and "queue it" vs "interrupt and
  // resend" is exactly the pair no mis-tap may confuse. Writing the message is the intent - the button that
  // acts on it appears with it, and never otherwise.
  interrupt.classList.toggle('hidden', !r || !has);
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

// send and interruptSend move the view only when the transcript is following the tail. A parked reader is
// reading a page that is not the end of the conversation, and sending a message from there is not a request
// to be taken somewhere else: the server queues the message and injects it at the next turn boundary, while
// the page in front of them stays exactly where they left it. Coming back is their gesture — the "newer"
// chip, dragging the transcript to its bottom, or Escape — and measuring it was the reason this was written
// down: the unforced version moved the viewport 205 870 pixels to the bottom of the parked page, which is
// the closest thing this UI has to a message that vanished.
async function send() {
  const body = draft();
  if (!body || !state.id) return;
  const id = state.id; // the reply belongs to the session that was on screen when Enter was pressed
  const wasHeld = state.status?.queued || 0; // what this tab last saw is the only baseline it has
  clearDraft();
  const parked = isParked(); // why: parked is "I am reading somewhere else", not "follow the tail"
  if (!parked) markPinned(); // sending a message means "show me the answer", before the server has anything to show
  const res = await call('/send', body);
  rememberHeld(id, body, res, wasHeld);
  if (!parked) scrollDown(true); // why: forced, and force is what an explicit press of the button means here
}

// rememberHeld notices what the server did with the message instead of guessing it from the phase. The reply
// carries the session's whole status, and a `queued` that went up is the fact that the message is being held
// off the path: a tool call is owed an answer and appending it now would put it between that call and its
// result (js/park). The phase would be the wrong thing to read twice over — a run can be live with nothing
// owed, and a session asleep on a wait has no run at all — and this tab has no way to know the queue depth
// any other way, because holding a message emits no event.
//
// It draws nothing itself beyond handing that status to the renderer, which owns both halves of the box
// (js/session.js renderHeld): this module knows the words, and only the count is the server's.
function rememberHeld(id, body, res, wasHeld) {
  const status = res?.status;
  // why the id check: the Enter was pressed on one session and the reply can land after a click moved the
  // tab to another, and a message belongs to the conversation it was typed into.
  if (!status || state.id !== id) return;
  state.status = status;
  if ((status.queued || 0) > wasHeld) {
    state.held.push({ text: body.text || '', images: (body.images || []).length });
  }
  onStatus?.(status);
}

async function interruptSend() {
  const body = draft();
  if (!body || !state.id) return;
  clearDraft();
  const parked = isParked(); // why: the bolt aborts a generation; it does not get to move the reader
  if (!parked) markPinned();
  // /interrupt answers `{ok}` and nothing else, so this tab learns nothing here about where the message
  // went: unlike a send there is no status to take a held count from, and the held box (js/session.js
  // renderHeld) has to wait for the next status of the session to notice this message at all.
  await call('/interrupt', body);
  if (!parked) scrollDown(true);
}

async function stop() {
  if (!state.id) return;
  await call('/stop', {});
}

// onStatus is handed in rather than imported. This module already sits on the receiving end of an import
// from js/session.js (which owns syncComposer), and a composer that imported back would close the project's
// first module cycle just to reach one renderer — app.js has both of them in scope already and can hand the
// function over. It exists because the reply to a send is the freshest status this tab ever gets at the
// moment it most needs one: see rememberHeld.
let onStatus = null;

export function wireComposer(hooks = {}) {
  onStatus = hooks.onStatus || null;
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
    send(); // one keystroke, one meaning, in every phase: mid-run the endpoint appends the message and the
    // loop picks it up at the next turn boundary. Aborting the generation in progress is the bolt's job —
    // a keystroke that quietly threw away half an answer was never what "send" was for.
  });
  input().addEventListener('paste', (e) => {
    const items = [...(e.clipboardData?.items || [])].filter((i) => i.type.startsWith('image/'));
    if (!items.length) return;
    e.preventDefault();
    addFiles(items.map((i) => i.getAsFile()).filter(Boolean));
  });
}
