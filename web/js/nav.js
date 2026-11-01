// The session sidebar has three shapes and this module is the switch between them.
//
//   shape          when                                  what `nav-open` means
//   column         desktop window, not collapsed         nothing: the column is the layout
//   drawer         ≤ 860 px (app.css owns this)          slide the column in over the transcript
//   panel          desktop window, collapsed             drop the list from under ☰
//
// Which shape is in effect is a pair of classes on <body>, so app.css answers the layout question and this
// file only ever answers "what did the reader just ask for". `nav-open` is "the sidebar is where it is not
// normally"; `nav-collapsed` is "on a desktop window this app does not reserve a column for the sidebar".
// Below the breakpoint the collapse is inert - the media query owns the whole layout there - which is why a
// stored collapse is *not applied* on a narrow window rather than being forgotten.
//
// The collapse is a preference of this browser, so it is stored in localStorage like the render rate and the
// enter-sends switch. Where a session sits in the list is the opposite kind of fact and lives in the
// session's own config.json: the same person on another machine sees the same pinned and archived sessions
// and may well want the sidebar wide over there.
import { $ } from './util.js';

const KEY = 'bareloop.navCollapsed';

// The same 860px app.css asks for the drawer, written down twice because there is no build step to share
// one number between a stylesheet and a module. It decides two things only: whether a stored collapse
// applies, and whether focusing the filter would be a kindness or a keyboard over the transcript. The
// layout is answered by the media query, so a disagreement here could cost a class and never a pixel.
const MOBILE_PX = 860;

let open = false;

export const isMobile = () => window.matchMedia(`(max-width: ${MOBILE_PX}px)`).matches;
export const isCollapsed = () => document.body.classList.contains('nav-collapsed');

// setDrawer is the difference between the two floating shapes and the one that is part of the grid: the
// class carries the box (fixed, 70vh, under the top bar) and `nav-open` decides whether it is painted.
function setDrawer(on) {
  const side = $('#side');
  if (side) side.classList.toggle('drawer', on && !isMobile());
}

export function setNav(isOpen) {
  open = isOpen;
  document.body.classList.toggle('nav-open', isOpen);
  const scrim = $('#scrim');
  if (scrim) scrim.classList.toggle('hidden', !isOpen);
  // A panel that opens and then wants a second click before you can type is not a panel. On a phone the
  // same focus would shove the keypad over the drawer the reader was about to scan, so it is desktop-only.
  if (isOpen && isCollapsed() && !isMobile()) $('#session-filter')?.focus();
}

export const closeNav = () => {
  if (open) setNav(false);
};

export const navIsOpen = () => open;

// setCollapsed is the whole of the preference: one class, one drawer flag, one stored byte. `remember` is
// for the two callers that are a reader crossing the breakpoint by themselves; the ones that are this
// module reacting to a resize do not write anything, because a window that is temporarily narrow did not
// change anybody's mind.
function setCollapsed(on, remember) {
  document.body.classList.toggle('nav-collapsed', on);
  setDrawer(on);
  syncCollapseButton();
  if (!remember) return;
  try {
    if (on) localStorage.setItem(KEY, '1');
    else localStorage.removeItem(KEY);
  } catch {
    // A browser that refuses storage (private mode, blocked cookies) still gets a collapse that works until
    // the reload - which is the same deal every other remembered preference in this app makes.
  }
}

// The button that owns the collapse lives inside the sidebar, where the thing it moves is. One control and
// one meaning: the arrow says which way the column goes, and it is hidden below the breakpoint because a
// phone window has no column to give back.
function syncCollapseButton() {
  const btn = $('#collapse');
  if (!btn) return;
  const on = isCollapsed();
  btn.title = on ? 'keep the session sidebar open' : 'collapse the session sidebar and widen the transcript';
  btn.setAttribute('aria-label', btn.title);
  btn.setAttribute('aria-expanded', String(on));
}

// bootNav runs before the first paint of the sidebar, so a window that was collapsed last time never shows
// 210px of sidebar and then loses it.
export function bootNav() {
  let saved = null;
  try {
    saved = localStorage.getItem(KEY);
  } catch {
    saved = null;
  }
  setCollapsed(saved === '1' && !isMobile(), false);

  $('#menu').onclick = () => setNav(!open);
  $('#scrim').onclick = () => setNav(false);
  const btn = $('#collapse');
  if (btn) {
    btn.onclick = () => {
      // Opening and collapsing are never both true: the drawer is the collapsed shape's way of being open,
      // so asking to collapse while it is open is asking for the panel, and asking to expand while the panel
      // is open is asking for the column.
      const on = !isCollapsed();
      setNav(false);
      setCollapsed(on, true);
    };
  }
  // Crossing the breakpoint in either direction is a relayout rather than an edit of the drawer. Held
  // across it, the class would be a desktop window with no column reserved and no panel to open - a sidebar
  // that cannot be opened - and a phone drawer left "open" would come back as a panel when the window grew.
  // The stored value is left alone, so growing the window back restores the choice.
  window.addEventListener('resize', () => {
    setCollapsed(isCollapsed() && !isMobile(), false);
    setNav(false);
  });
}
