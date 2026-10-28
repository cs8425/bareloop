// Small DOM and formatting helpers shared by every UI module. Nothing here knows about the
// transcript, the API or the session — if a helper needs those, it belongs in a bigger module.

export const $ = (sel) => document.querySelector(sel);

export const el = (tag, cls, text) => {
  const node = document.createElement(tag);
  if (cls) node.className = cls;
  if (text !== undefined) node.textContent = text;
  return node;
};

// logEl is the transcript box. It is looked up once per call rather than cached, so a module never
// holds a reference to a node that a later refactor replaces.
export const logEl = () => $('#log');

// 1234 -> "1.2k": token counts are read at a glance, so precision past two digits is noise.
export const fmtN = (n) => (n >= 1000 ? (n / 1000).toFixed(n >= 10000 ? 0 : 1) + 'k' : String(n ?? 0));

export const clamp = (v, lo, hi) => Math.max(lo, Math.min(hi, v));

// pretty formats JSON for display, and passes through anything that is not JSON — a tool may well
// return text, and failing to parse it is not an error worth showing.
export const pretty = (text) => {
  if (!text) return '';
  try {
    return JSON.stringify(JSON.parse(text), null, 2);
  } catch {
    return text;
  }
};

// collapseDataUrls swaps every inline data: URL in a piece of text for a one-line marker and hands
// back the images it found. A quarter megabyte of base64 in a <pre> is not readable, it makes the
// transcript unscrollable and it freezes the tab; an image is something to look at, not to read.
const DATA_URL = /data:([\w.+-]+\/[\w.+-]+);base64,([A-Za-z0-9+/\s=]+)/g;

export function collapseDataUrls(text) {
  const images = [];
  if (!text || typeof text !== 'string') return { text: text || '', images };
  const out = text.replace(DATA_URL, (whole, mime, data) => {
    images.push({ mimeType: mime, data });
    return `[${mime}, ~${Math.round((data.length * 3) / 4 / 1024)} KB of base64 not shown]`;
  });
  return { text: out, images };
}

// Entry ids are zero-padded decimal strings, which sort as strings only while the width holds.
export const numId = (id) => {
  const n = Number(id);
  return Number.isFinite(n) ? n : 0;
};
export const sortByNum = (list) => list.slice().sort((a, b) => numId(a.id) - numId(b.id));

// pathOf walks parents from the tip and returns the ids that are actually sent: everything else is
// history on another branch, and with no tree view to return to it the transcript does not draw it.
export function pathOf(entries, tip) {
  const byId = new Map(entries.map((e) => [e.id, e]));
  const set = new Set();
  let cur = tip;
  while (cur && byId.has(cur)) {
    set.add(cur);
    cur = byId.get(cur).parentId;
  }
  return set;
}

export const whoName = (e) => (e.kind === 'compaction' ? 'compaction' : e.role || e.kind);
