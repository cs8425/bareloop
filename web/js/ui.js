// Everything that speaks to the user outside the transcript: notes, toasts, and the two modal
// questions the app asks.
//
// There is a rule here, and it is deliberate: no window.alert / confirm / prompt. They freeze the
// page (no event runs while one is open, so a streaming answer stops rendering), they cannot be
// styled or read on a phone browser chrome, and their OK/Cancel labels are untranslatable and vague.
// Everything below is a <dialog>, which the browser already makes modal and focus-trapping.
import { $, el } from './util.js';
import { state } from './state.js';
import { scrollDown } from './scroll.js';

// note writes into the transcript: process chatter, compaction chatter, anything that belongs to the
// story being told in order.
export function note(text, kind = 'note') {
  if (!text) return;
  const log = $('#log');
  if (!log) return;
  const node = el(kind === 'error' ? 'div' : 'div', kind === 'error' ? 'msg error' : 'note', text);
  log.appendChild(node);
  scrollDown();
  if (kind === 'note') setTimeout(() => node.remove(), 9000);
}

// toast is for "this needs your eyes now", independent of where the transcript is scrolled to:
// a failed save is worthless if it lands sixty messages above the fold.
export function toast(text, kind = 'info') {
  if (!text) return;
  let host = $('#toasts');
  if (!host) {
    host = el('div');
    host.id = 'toasts';
    document.body.appendChild(host);
  }
  const node = el('div', kind === 'error' ? 'toast err' : 'toast', text);
  host.appendChild(node);
  // Long messages about a rejected config deserve the time it takes to read them.
  const ms = Math.min(12000, 3500 + 45 * String(text).length);
  setTimeout(() => {
    node.classList.add('out');
    setTimeout(() => node.remove(), 400);
  }, ms);
  return node;
}

// fail is the one shape every action uses: log it, show it, and rethrow so the caller can stop.
export function fail(err) {
  const msg = err && err.message ? err.message : String(err);
  note(msg, 'error');
  toast(msg, 'error');
  return msg;
}

// modal builds a <dialog> around some content and returns a promise of its returnValue. The form
// with method="dialog" is what makes a submit button close it, and Escape fires cancel — both of
// which the browser handles, so there is no keydown handler here to get wrong.
function modal({ title, body, actions, autofocus, focus = 'primary' }) {
  const dlg = el('dialog', 'modal');
  const form = el('form');
  form.method = 'dialog';
  if (title) form.appendChild(el('h3', '', title));
  if (body) form.appendChild(body);
  const row = el('menu');
  for (const a of actions) {
    const b = el('button', a.cls || '', a.label);
    b.value = a.value;
    b.type = 'submit';
    if (a.primary) b.classList.add('primary');
    row.appendChild(b);
  }
  form.appendChild(row);
  dlg.appendChild(form);
  document.body.appendChild(dlg);
  dlg.showModal();
  // Where the caret starts is the answer a stray Enter commits, so it is chosen, not inherited: an
  // input if there is one, the safe button when the alternative destroys something, the primary
  // button otherwise.
  const target =
    (autofocus && form.querySelector(`[name="${autofocus}"]`)) ||
    (focus === 'cancel' ? form.querySelector('button[value="cancel"]') : row.querySelector('.primary')) ||
    row.lastChild;
  target?.focus();
  return new Promise((resolve) => {
    dlg.addEventListener('close', () => {
      // Escape leaves returnValue empty: that is a cancel, not an answer.
      resolve(dlg.returnValue || 'cancel');
      dlg.remove();
    });
  });
}

// confirmBox replaces window.confirm. The default answer is "no": the button on the right is the
// safe one, and a stray Enter must not delete anything.
export function confirmBox(message, { title = 'Are you sure?', detail = '', ok = 'yes', cancel = 'cancel', danger = false } = {}) {
  const body = el('div');
  body.appendChild(el('p', '', message));
  if (detail) body.appendChild(el('p', 'hint', detail));
  return modal({
    title,
    body,
    focus: danger ? 'cancel' : 'primary',
    actions: [
      { value: 'cancel', label: cancel },
      { value: 'ok', label: ok, primary: !danger, cls: danger ? 'danger' : '' },
    ],
  }).then((answer) => answer === 'ok');
}

// promptBox replaces window.prompt and resolves null on cancel (not the empty string, which is a
// legitimate answer).
export function promptBox(message, { title = 'Input', value = '', placeholder = '', ok = 'ok', cancel = 'cancel' } = {}) {
  const body = el('div');
  body.appendChild(el('p', '', message));
  const field = el('input');
  field.name = 'answer';
  field.value = value;
  field.placeholder = placeholder;
  body.appendChild(field);
  return modal({
    title,
    body,
    autofocus: 'answer',
    actions: [
      { value: 'cancel', label: cancel },
      { value: 'ok', label: ok, primary: true },
    ],
  }).then((answer) => (answer === 'ok' ? field.value : null));
}

// setConn reports which transport is live: a websocket, the SSE fallback, or polling.
export function setConn(on, label, transport) {
  const dot = $('#conn');
  if (!dot) return;
  dot.classList.toggle('on', !!on);
  dot.classList.toggle('off', !on);
  $('#conn-label').textContent = label;
  state.transport = transport || 'none';
}
