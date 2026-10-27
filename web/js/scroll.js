// Auto-scroll, which is the one piece of this UI that has to fight the page.
//
// Following the transcript is opt-out, and the opt-out has to be instant: a wall of tokens must never
// drag the viewport away from the composer, or stop and interrupt become unreachable. The rule is
// "follow only while pinned", where pinned means the bottom of #log is on screen. Any scroll the
// user performs unpins; the floating jump button (or Escape) re-pins.
import { $, logEl } from './util.js';

const PIN_PX = 48;
let pinned = true;
let unseen = 0;
let lastScrollWrite = 0;

// A reload restores the old scroll offset of #log, which looks exactly like the user scrolling away
// from the bottom. On a fresh page the newest message is the right place, so opt out.
if ('scrollRestoration' in window.history) window.history.scrollRestoration = 'manual';

const distanceFromBottom = (box) => box.scrollHeight - box.scrollTop - box.clientHeight;

export const isPinned = () => pinned;

// markPinned re-arms the follow without moving anything: an action the user just took (sending a
// message) means the next tokens are wanted, but scrolling now would race the server's first delta.
export const markPinned = () => {
  pinned = true;
  unseen = 0;
  renderJump();
};

export function scrollDown(force = false) {
  const box = logEl();
  if (!pinned && !force) {
    unseen++;
    renderJump();
    return;
  }
  if (force) pinned = true;
  unseen = 0;
  lastScrollWrite = Date.now();
  box.scrollTop = box.scrollHeight;
  renderJump();
}

function renderJump() {
  const btn = $('#jump');
  if (!btn) return;
  btn.classList.toggle('hidden', pinned);
  const label = unseen > 0 ? `\u2193 ${unseen > 99 ? '99+' : unseen} new` : '\u2193 latest';
  if (btn.textContent.trim() !== label) btn.textContent = label;
}

// resetScroll re-arms the follow, e.g. after switching to another session.
export function resetScroll() {
  pinned = true;
  unseen = 0;
  renderJump();
}

// scrollsInside reports whether a nested scroll box (a tool card's <pre>) would consume the wheel
// movement, so scrolling a code block does not unpin the transcript.
function scrollsInside(target, up) {
  for (let n = target; n && n !== logEl(); n = n.parentElement) {
    if (n.scrollHeight - n.clientHeight <= 1) continue;
    const atTop = n.scrollTop <= 0;
    const atBottom = n.scrollTop + n.clientHeight >= n.scrollHeight - 1;
    if (up ? !atTop : !atBottom) return true;
  }
  return false;
}

export function wireScroll() {
  const box = logEl();
  box.addEventListener(
    'scroll',
    () => {
      if (Date.now() - lastScrollWrite < 150) return; // our own follow, not the user's scroll
      const atBottom = distanceFromBottom(box) <= PIN_PX;
      if (atBottom === pinned) return;
      pinned = atBottom;
      if (atBottom) unseen = 0;
      renderJump();
    },
    { passive: true },
  );
  // Wheeling up while already at the bottom fires no scroll event, so read the intent itself.
  box.addEventListener(
    'wheel',
    (e) => {
      if (e.deltaY >= 0 || !pinned || scrollsInside(e.target, true)) return;
      pinned = false;
      renderJump();
    },
    { passive: true },
  );
  $('#jump').onclick = () => scrollDown(true);
}
