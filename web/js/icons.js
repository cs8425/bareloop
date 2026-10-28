// One source of icons for the whole UI.
//
// The geometry follows Lucide (24x24, stroke 2, round caps, `currentColor`) because llama.cpp's own web
// UI uses the same set, and the two windows sit next to each other on the same desk: an icon that almost
// matches is worse than one that is obviously the same language. The stop square and the code brackets
// are the only shapes drawn by hand, and both are drawn to be read at 15px.
//
// Markup gets icons by name - `<span data-icon="send"></span>` - and `boot()` swaps the placeholder for
// the real svg, keeping the attributes it finds on the placeholder. That keeps every glyph defined once,
// and a button never depends on a copy of a path in index.html.
const PATHS = {
  send: '<path d="m5 12 7-7 7 7"/><path d="M12 19V5"/>',
  stop: '<rect x="6" y="6" width="12" height="12" rx="2.5" fill="currentColor" stroke="none"/>',
  interrupt: '<path d="M13 2 4.4 13.2a.6.6 0 0 0 .47.98H10l-1 8 8.6-11.2a.6.6 0 0 0-.47-.98H12l1-8z" fill="currentColor" stroke="none"/>',
  plus: '<path d="M5 12h14"/><path d="M12 5v14"/>',
  copy: '<rect width="14" height="14" x="8" y="8" rx="2" ry="2"/><path d="M4 16c-1.1 0-2-.9-2-2V4c0-1.1.9-2 2-2h10c1.1 0 2 .9 2 2"/>',
  check: '<path d="M20 6 9 17l-5-5"/>',
  branch: '<path d="M15 6a9 9 0 0 0-9 9V3"/><circle cx="18" cy="6" r="3"/><circle cx="6" cy="18" r="3"/>',
  restart:
    '<path d="M3 12a9 9 0 0 1 9-9 9.75 9.75 0 0 1 6.74 2.74L21 8"/><path d="M21 3v5h-5"/><path d="M21 12a9 9 0 0 1-9 9 9.75 9.75 0 0 1-6.74-2.74L3 16"/><path d="M8 16H3v5"/>',
  trash:
    '<path d="M10 11v6"/><path d="M14 11v6"/><path d="M19 6v14a2 2 0 0 1-2 2H7a2 2 0 0 1-2-2V6"/><path d="M3 6h18"/><path d="M8 6V4a2 2 0 0 1 2-2h4a2 2 0 0 1 2 2v2"/>',
  raw: '<path d="m16 18 6-6-6-6"/><path d="m8 6-6 6 6 6"/>',
  inspect: '<circle cx="11" cy="11" r="8"/><path d="m21 21-4.3-4.3"/>',
  compact:
    '<path d="M12 3v6"/><path d="m9 6 3 3 3-3"/><path d="M12 21v-6"/><path d="m9 18 3-3 3 3"/><path d="M4 12h16"/>',
  settings:
    '<path d="M9.671 4.136a2.34 2.34 0 0 1 4.659 0 2.34 2.34 0 0 0 3.319 1.915 2.34 2.34 0 0 1 2.33 4.033 2.34 2.34 0 0 0 0 3.831 2.34 2.34 0 0 1-2.33 4.033 2.34 2.34 0 0 0-3.319 1.915 2.34 2.34 0 0 1-4.659 0 2.34 2.34 0 0 0-3.32-1.915 2.34 2.34 0 0 1-2.33-4.033 2.34 2.34 0 0 0 0-3.831A2.34 2.34 0 0 1 6.35 6.051a2.34 2.34 0 0 0 3.319-1.915"/><circle cx="12" cy="12" r="3"/>',
  menu: '<path d="M4 6h16"/><path d="M4 12h16"/><path d="M4 18h16"/>',
  close: '<path d="M18 6 6 18"/><path d="m6 6 12 12"/>',
  down: '<path d="m19 12-7 7-7-7"/><path d="M12 5v14"/>',
};

// icon returns a standalone <svg>. It is not `aria-hidden` only when given no name to label it with:
// a button whose whole content is this svg still has to say what it does, and that is what the caller's
// aria-label is for.
export function icon(name, cls = "") {
  const svg = document.createElementNS("http://www.w3.org/2000/svg", "svg");
  svg.setAttribute("viewBox", "0 0 24 24");
  svg.setAttribute("fill", "none");
  svg.setAttribute("stroke", "currentColor");
  svg.setAttribute("stroke-width", "2");
  svg.setAttribute("stroke-linecap", "round");
  svg.setAttribute("stroke-linejoin", "round");
  svg.setAttribute("aria-hidden", "true");
  svg.setAttribute("focusable", "false");
  if (cls) svg.setAttribute("class", cls);
  svg.innerHTML = PATHS[name] || '<circle cx="12" cy="12" r="9" />'; // a missing name shows as a dot, not as nothing
  return svg;
}

// boot replaces every `data-icon` placeholder with its svg, once, at startup. The placeholder keeps the
// markup readable (`<button data-icon="send" aria-label="send">`) and costs nothing if the script fails:
// what is left behind is a button with its accessible name.
export function bootIcons(root = document) {
  for (const node of root.querySelectorAll("[data-icon]")) {
    const svg = icon(node.dataset.icon, node.className || "");
    // The placeholder may carry an id, a class and the accessible name; all of it moves over, because
    // listeners and styles reach for `#btn-send` and `.act`, not for a span that no longer exists.
    svg.setAttribute("class", [node.className, "ico"].filter(Boolean).join(" "));
    for (const attr of node.attributes) {
      if (attr.name !== "class" && attr.name !== "data-icon" && !svg.hasAttribute(attr.name)) {
        svg.setAttribute(attr.name, attr.value);
      }
    }
    node.replaceWith(svg);
  }
}

// swapIcon shows `name` in place of the button's current glyph, then puts `back` there a moment later:
// the copy button that turns into a tick for a second says "that worked" without stealing space for a
// toast. Both are drawn with the existing class, so the size never changes underneath a click.
export function swapIcon(button, name, back, ms = 1200) {
  const at = button.querySelector("svg");
  if (!at) return;
  const cls = at.getAttribute("class") || "";
  button.replaceChild(icon(name, cls), at);
  if (!back) return;
  setTimeout(() => {
    const now = button.querySelector("svg");
    if (now) button.replaceChild(icon(back, cls), now);
  }, ms);
}
