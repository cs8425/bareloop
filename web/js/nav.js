// The session drawer on phones. Below the CSS breakpoint the sidebar is off-canvas and ☰ slides it
// in over the transcript; on a desktop viewport ☰ and the scrim are display:none, so none of this is
// reachable and the class it toggles is inert.
import { $ } from './util.js';

let open = false;

export function setNav(isOpen) {
  open = isOpen;
  document.body.classList.toggle('nav-open', isOpen);
  const scrim = $('#scrim');
  if (scrim) scrim.classList.toggle('hidden', !isOpen);
}

export const closeNav = () => {
  if (open) setNav(false);
};

export const navIsOpen = () => open;

export function wireNav() {
  $('#menu').onclick = () => setNav(!open);
  $('#scrim').onclick = () => setNav(false);
}
