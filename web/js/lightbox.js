// Zooming a picture. An attachment in the transcript is drawn at 220×160 (web/app.css), which is a thumbnail
// of a screenshot and about as readable as one, and the log keeps the whole original — the model sees more of
// it than the reader does. So every image in the transcript answers a click by opening nearly the whole window
// and letting the reader ask for more: wheel and pinch to scale around the point under the pointer, drag to
// move, double-click to swap between fitting and 1:1, arrows to walk the pictures of the same message.
//
// One delegated listener on `#log`, rather than wiring each image as it is drawn: the transcript is rebuilt on
// every refresh, and a click handler that has to be re-attached to every entry is a handler that is missing on
// the entries drawn before it was written.
//
// What is deliberately not here: editing, cropping, uploading, downloading. This is a viewer for bytes the log
// already has — and a data: URL is not a file the browser would let us save under its own name.
import { $, el } from './util.js';

// What counts as a zoomable picture. `.attach` is an attachment's own <img> (js/transcript.js) and a tool
// output's; `.md img` is one the reply itself linked to. Both are drawn small, both are the whole file.
const IMG = 'img.attach, .md img';

const MIN = 0.1;
const MAX = 12;
// A wheel notch is a factor, not a number of pixels, so one notch feels the same at 20% as at 800%.
const WHEEL_STEP = 1.12;

// One viewer, opened on demand. `items` are the images of the clicked entry (one when the click landed on a
// markdown image with no siblings), `at` is the one that was clicked.
let items = [];
let at = 0;
let dlg = null;
let stage = null;
let img = null;
let readout = null;
let dimEl = null;
let nameEl = null;
// The picture's own transform. Everything else in the dialog is static, so the whole viewer state is these
// three numbers — and `fit` is not one of them, because it has to be recomputed whenever the window is resized.
let scale = 1;
let tx = 0;
let ty = 0;
let fitted = true;

function apply() {
  img.style.transform = `translate(${tx}px, ${ty}px) scale(${scale})`;
  if (readout) readout.textContent = `${Math.round(scale * 100)}%`;
  // The buttons are a mode indicator as much as a control: 1:1 is only worth highlighting when that is what
  // the reader is looking at, and a control that lies about the state it did not cause is how a reader ends up
  // pressing it twice.
  const one = Math.abs(scale - 1) < 0.005;
  $('#zoom-1')?.classList.toggle('on', one);
  $('#zoom-fit')?.classList.toggle('on', fitted && !one);
}

const fitScale = () => {
  if (!img.naturalWidth || !stage.clientWidth || !stage.clientHeight) return 1;
  return Math.min(stage.clientWidth / img.naturalWidth, stage.clientHeight / img.naturalHeight, 1);
};

function fit() {
  scale = fitScale();
  tx = 0;
  ty = 0;
  fitted = true;
  apply();
}

// Zoom keeping one screen point pinned — the pointer for a wheel, the centre for a button. Without the pin a
// zoom at 400% moves the detail the reader was reading off screen, which turns zooming into scrolling.
function zoomAt(next, cx = 0, cy = 0) {
  const clamped = Math.min(MAX, Math.max(MIN, next));
  if (clamped === scale) return;
  const k = clamped / scale;
  tx = cx - (cx - tx) * k;
  ty = cy - (cy - ty) * k;
  scale = clamped;
  fitted = false;
  apply();
}

// The point a wheel or a touch is anchored at, in the stage's own coordinates with the centre as origin —
// which is exactly what `apply` writes its transform against (transform-origin is the centre).
function localPoint(clientX, clientY) {
  const r = stage.getBoundingClientRect();
  return { x: clientX - r.left - r.width / 2, y: clientY - r.top - r.height / 2 };
}

function show(i) {
  if (!items.length) return;
  at = (i + items.length) % items.length;
  const it = items[at];
  img.src = it.src;
  img.alt = it.name || 'image';
  if (nameEl) nameEl.textContent = items.length > 1 ? `${it.name || 'image'} · ${at + 1}/${items.length}` : it.name || '';
  // The real pixel size is the one fact a reader cannot get from a thumbnail, and 1:1 means something only
  // next to it ("1600×900 · 13% of the window" is why the zoom exists).
  dimEl.textContent = '';
  img.onload = () => {
    dimEl.textContent = `${img.naturalWidth}×${img.naturalHeight}`;
    fit();
  };
  const many = items.length > 1;
  $('#zoom-prev').classList.toggle('hidden', !many);
  $('#zoom-next').classList.toggle('hidden', !many);
}

function open(src, name, siblings, index) {
  if (!dlg) build();
  items = siblings.length ? siblings : [{ src, name }];
  show(Math.max(0, index));
  dlg.showModal();
}

function build() {
  dlg = el('dialog', 'zoom');
  dlg.id = 'dlg-zoom';
  const bar = el('div', 'zoom-bar');
  nameEl = el('span', 'zoom-name');
  dimEl = el('span', 'zoom-dim');
  readout = el('span', 'zoom-scale', '100%');
  const btn = (id, glyph, title, fn) => {
    const b = el('button', 'icon-btn');
    b.id = id;
    b.type = 'button';
    b.textContent = glyph;
    b.title = b.ariaLabel = title;
    b.onclick = fn;
    return b;
  };
  const spacer = el('span', 'zoom-spacer');
  bar.append(
    nameEl,
    dimEl,
    readout,
    spacer,
    btn('zoom-out', '−', 'zoom out', () => zoomAt(scale / WHEEL_STEP ** 2)),
    btn('zoom-in', '+', 'zoom in', () => zoomAt(scale * WHEEL_STEP ** 2)),
    btn('zoom-fit', '⤢', 'fit this window to the picture', fit),
    btn('zoom-1', '1:1', 'show it at one screen pixel per image pixel', () => {
      scale = 1;
      fitted = false;
      apply();
    }),
    btn('zoom-prev', '←', 'previous image of this message', () => show(at - 1)),
    btn('zoom-next', '→', 'next image of this message', () => show(at + 1)),
    btn('zoom-close', '×', 'close (Esc)', () => dlg.close()),
  );
  stage = el('div', 'zoom-stage');
  stage.id = 'zoom-stage';
  img = el('img');
  img.id = 'zoom-img';
  stage.appendChild(img);
  dlg.append(bar, stage);
  document.body.appendChild(dlg);

  // Wheel: zoom at the pointer; shift-wheel and horizontal wheels pan, because a trackpad sends both and a
  // two-finger swipe that zooms a screenshot sideways is the gesture nobody asked for.
  stage.addEventListener(
    'wheel',
    (e) => {
      e.preventDefault();
      const p = localPoint(e.clientX, e.clientY);
      if (e.shiftKey || Math.abs(e.deltaX) > Math.abs(e.deltaY)) {
        tx -= (e.deltaX || e.deltaY);
        ty -= e.deltaX ? e.deltaY : 0;
        fitted = false;
        apply();
        return;
      }
      zoomAt(scale * (e.deltaY < 0 ? WHEEL_STEP : 1 / WHEEL_STEP), p.x, p.y);
    },
    { passive: false },
  );

  // Drag with a pointer, two fingers on a trackpad that sends no wheel lines. setPointerCapture is what lets
  // the drag survive the pointer leaving the stage — and it is why no document-level listener is needed.
  let drag = null;
  // Two pointers is a pinch, and a pinch that also drags is how every image viewer on the phone behaves. The
  // gesture is the distance between the fingers and the point between them, and both are remembered because a
  // scale has to be applied relative to what was true on the previous move.
  let pinch = null;
  const touches = new Map();
  stage.addEventListener('pointerdown', (e) => {
    if (e.pointerType === 'mouse' && e.button !== 0) return;
    touches.set(e.pointerId, { x: e.clientX, y: e.clientY });
    stage.setPointerCapture(e.pointerId);
    if (touches.size === 2) {
      const [a, b] = [...touches.values()];
      pinch = { d: Math.hypot(a.x - b.x, a.y - b.y) || 1, x: (a.x + b.x) / 2, y: (a.y + b.y) / 2 };
      drag = null;
      return;
    }
    drag = { x: e.clientX, y: e.clientY, tx, ty, moved: false };
  });
  stage.addEventListener('pointermove', (e) => {
    if (touches.has(e.pointerId)) touches.set(e.pointerId, { x: e.clientX, y: e.clientY });
    if (pinch && touches.size === 2) {
      const [a, b] = [...touches.values()];
      const d = Math.hypot(a.x - b.x, a.y - b.y) || 1;
      const mid = localPoint((a.x + b.x) / 2, (a.y + b.y) / 2);
      zoomAt((scale * d) / pinch.d, mid.x, mid.y);
      pinch = { d, x: (a.x + b.x) / 2, y: (a.y + b.y) / 2 };
      return;
    }
    if (!drag) return;
    const dx = e.clientX - drag.x;
    const dy = e.clientY - drag.y;
    // One pixel of movement is a drag; zero is a click, and a click on the backdrop closes the viewer. The
    // distinction is what lets the same surface be both the thing you move and the way out.
    if (Math.abs(dx) + Math.abs(dy) > 2) drag.moved = true;
    if (!drag.moved) return;
    stage.classList.add('dragging');
    tx = drag.tx + dx;
    ty = drag.ty + dy;
    fitted = false;
    apply();
  });
  const up = (e) => {
    touches.delete(e.pointerId);
    if (touches.size < 2) pinch = null;
    if (!drag) return;
    const wasStill = !drag.moved;
    drag = null;
    stage.classList.remove('dragging');
    if (wasStill && e.target === stage) dlg.close(); // the backdrop, not the picture
  };
  stage.addEventListener('pointerup', up);
  stage.addEventListener('pointercancel', up);
  stage.addEventListener('dblclick', (e) => {
    const p = localPoint(e.clientX, e.clientY);
    if (fitted || Math.abs(scale - 1) > 0.005) zoomAt(1, p.x, p.y);
    else fit();
  });
  // Escape belongs to <dialog> and it already closes it. These are the keys that answer while the pointer is
  // somewhere else — and `+`/`-`/`0` are the ones a reader finds by trying, on a page where everything else is
  // a button.
  dlg.addEventListener('keydown', (e) => {
    if (e.key === '+' || e.key === '=') zoomAt(scale * WHEEL_STEP ** 2);
    else if (e.key === '-' || e.key === '_') zoomAt(scale / WHEEL_STEP ** 2);
    else if (e.key === '0') fit();
    else if (e.key === '1') {
      scale = 1;
      fitted = false;
      apply();
    } else if (e.key === 'ArrowLeft') show(at - 1);
    else if (e.key === 'ArrowRight') show(at + 1);
    else return;
    e.preventDefault();
  });
  // The window is the thing that changed size, and the picture's whole job is to fit it.
  window.addEventListener('resize', () => {
    if (dlg.open && fitted) fit();
  });
}

// The pictures of the message the click landed in, in document order — so ← then → walks the attachments the
// way they were sent, which is the order the assembler put on the wire.
function siblingsOf(node) {
  const entry = node.closest('.msg, .tool-card, .body');
  const pool = entry || node.parentElement;
  const list = [...(pool || document).querySelectorAll(IMG)];
  return list.map((n) => ({ src: n.src, name: n.getAttribute('alt') || 'image', node: n }));
}

export function wireZoom() {
  // Capture phase on #log: the transcript is emptied and rebuilt by every refresh, and a listener on the box
  // itself outlives every entry drawn inside it.
  const box = document.getElementById('log');
  if (!box) return;
  box.addEventListener('click', (e) => {
    const node = e.target;
    if (!(node instanceof HTMLImageElement) || !node.matches(IMG)) return;
    const list = siblingsOf(node);
    const index = Math.max(0, list.findIndex((it) => it.node === node));
    open(node.src, node.getAttribute('alt') || 'image', list, index);
  });
}
