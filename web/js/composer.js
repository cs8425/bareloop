// The composer: the textarea, its attachments, and the three buttons that talk to the loop.
//
// Enter sends, Shift+Enter is a newline, and Enter while a run is live *interrupts* and sends
// instead — the interrupt endpoint writes the message and aborts the current call, so the turn is
// redirected rather than queued behind an answer nobody wants any more.
import { $, el } from './util.js';
import { state } from './state.js';
import { call } from './api.js';
import { note } from './ui.js';
import { markPinned, scrollDown } from './scroll.js';

const input = () => $('#input');

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
  $('#btn-send').onclick = send;
  $('#btn-interrupt').onclick = interruptSend;
  $('#btn-stop').onclick = stop;
  $('#files').onchange = (e) => {
    addFiles(e.target.files);
    e.target.value = '';
  };
  // Dropping an image anywhere in the composer is the same as pasting one; dragover has to be
  // cancelled or the browser navigates to the file instead.
  const composer = $('#composer');
  composer.addEventListener('dragover', (e) => e.preventDefault());
  composer.addEventListener('drop', (e) => {
    e.preventDefault();
    addFiles([...(e.dataTransfer?.files || [])]);
  });
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
}
