// Auto-scroll, which is the one piece of this UI that has to fight the page.
//
// Following the transcript is opt-out, and the opt-out has to be instant: a wall of tokens must never
// drag the viewport away from the composer, or stop and interrupt become unreachable. The rule is
// "follow only while pinned", where pinned means the bottom of #log is on screen. Any scroll the
// user performs unpins; the floating jump button (or Escape) re-pins.
import { $, logEl } from './util.js';
import { isParked } from './state.js';

const PIN_PX = 48;
let pinned = true;
let unseen = 0;
let lastScrollWrite = 0;
// The exact offset our own follow asked for. `lastScrollWrite` alone is a timer, and a timer loses to a busy
// main thread: a follow that lands while a large image is being decoded can have its own `scroll` event
// delivered more than 150 ms later, by which time the picture has grown the page below the viewport — and
// the handler reads that as "the reader scrolled away" and stops following for the rest of the run. Images
// make it a coin toss (which is exactly how it was reported), but the rule was always wrong: our write is
// identified by the position it wrote, and the timer is only the fall-back for a write that did not move.
let lastScrollTop = -1;

// A reload restores the old scroll offset of #log, which looks exactly like the user scrolling away
// from the bottom. On a fresh page the newest message is the right place, so opt out.
if ('scrollRestoration' in window.history) window.history.scrollRestoration = 'manual';

// distanceFromBottom is exported for the one other module that has to know how far the viewport is from the
// end of the transcript: the window pager uses it to decide whether a reader has come back for the newest
// part, and it needs the distance rather than the yes/no so it can make the reader clear some slack before
// the edge fires again.
export const distanceFromBottom = (box) => box.scrollHeight - box.scrollTop - box.clientHeight;

export const isPinned = () => pinned;

// atTheBottom answers "is the bottom of the transcript on screen" for someone else's decision, without
// exposing the pinned flag itself: a caller that wants to know whether the reader is at the end should not
// get to rewrite what following the end means. This reads the layout, so call it behind a cheap guard.
export const atTheBottom = () => distanceFromBottom(logEl()) <= PIN_PX;

// markPinned re-arms the follow without moving anything: an action the user just took (sending a
// message) means the next tokens are wanted, but scrolling now would race the server's first delta.
export const markPinned = () => {
  pinned = true;
  unseen = 0;
  renderJump();
};

export function scrollDown(force = false) {
  const box = logEl();
  // A parked transcript has no "down" worth going to: its bottom is a page boundary, not the newest
  // message. An unforced follow — a line of daemon chatter that appended a note, a piece of compaction
  // talk — must not drag a reader who is three pages back to the end of the page they are on. Forced
  // scrolls stay: that is a button being pressed, and the parked chips land themselves (js/session.js).
  if (isParked() && !force) return;
  if (!pinned && !force) {
    unseen++;
    renderJump();
    return;
  }
  if (force) pinned = true;
  unseen = 0;
  lastScrollWrite = Date.now();
  box.scrollTop = box.scrollHeight;
  // Read back, not assumed: a box already at its bottom clamps the write, and what the handler has to
  // recognise is the offset the browser settled on, not the one we asked for.
  lastScrollTop = box.scrollTop;
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
      // Our own follow, not the user's scroll — by offset first (it survives a late delivery) and by time
      // as a fall-back (a write that moved nothing produces no offset to match).
      if (box.scrollTop === lastScrollTop) return;
      if (Date.now() - lastScrollWrite < 150) return;
      const atBottom = distanceFromBottom(box) <= PIN_PX;
      if (atBottom === pinned) return;
      pinned = atBottom;
      if (atBottom) unseen = 0;
      renderJump();
    },
    { passive: true },
  );
  // A picture arrives after the text that asked for it, and it arrives with a height the transcript did not
  // budget: the follow scrolled to what was the bottom, then the image grew the page below it and "latest"
  // looks like it stopped working — until the next token scrolls it again, which on a finished answer may
  // never come. `load` does not bubble, but it does run through the capture phase, so one listener on the
  // box sees every image inside it. scrollDown() is the only hand on the viewport and it already refuses to
  // follow a reader who scrolled away, or to move a parked page, so asking here costs nothing when the
  // answer is no.
  box.addEventListener(
    'load',
    (e) => {
      if (e.target instanceof HTMLImageElement) scrollDown();
    },
    true,
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
