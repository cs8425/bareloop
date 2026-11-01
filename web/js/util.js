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

// A size in the unit you would say out loud. The byte floor never rounds to "0 KB": a page that
// fetched something and reports nothing is a rounding artefact no reader can tell from a real zero,
// and this number is quoted next to "how much of the session is on screen".
export const fmtBytes = (n) => {
  const b = n || 0;
  return b < 1024 ? `${b} B` : b < 1024 * 1024 ? `${Math.max(1, Math.round(b / 1024))} KB` : `${(b / (1024 * 1024)).toFixed(1)} MB`;
};

// How long ago is still "just now", and for how long a minute is the finest unit worth printing. A session
// list is scanned, not read: a minute-resolution figure that turns into "46m" is what tells you which of
// eleven rows you were last looking at.
const NOW_S = 60;
const MINUTE_S = 60 * NOW_S;
const HOUR_S = 60 * MINUTE_S;
const DAY_S = 24 * HOUR_S;

// fmtAgo is a session's age in the one unit that fits in a sidebar: `now`, `46m`, `3h`, `12d`. Past a day
// it gives up on being an age and gives the date ("10/3"), because "91d" would be a number a reader has to
// subtract from today to learn anything. The reference is `Date.now()`, so it is a wall-clock age and not
// the age of this tab's last load; null, an unparseable string and a timestamp in the future all come back
// empty — the last one because the clock of the daemon and the clock of this browser are not the same clock,
// and "-3m" next to a session is a claim about a time machine.
//
// It is deliberately not live: nothing re-renders the list to watch a minute go by, so the figure is "how
// old this was when the list was read". The full timestamp is in the row's title, which is the other half.
export const fmtAgo = (iso) => {
  if (!iso) return '';
  const then = Date.parse(iso);
  if (!Number.isFinite(then)) return '';
  const s = Math.round((Date.now() - then) / 1000);
  if (s < 0) return '';
  if (s < NOW_S) return 'now';
  if (s < MINUTE_S) return `${Math.round(s / 60)}m`;
  // Floor, not round, from here up: "24h" next to a session that has been sitting for a day is the sort of
  // rounding that makes two rows in one list disagree about what a day is.
  if (s < HOUR_S) return `${Math.floor(s / MINUTE_S)}h`;
  if (s < DAY_S) return `${Math.floor(s / HOUR_S)}d`;
  // The date, in the browser's own calendar: this figure is a hint about which row to click, and the month
  // and day are what a reader has of a session they have not opened since spring.
  const d = new Date(then);
  if (Number.isNaN(d.getTime())) return '';
  return `${d.getMonth() + 1}/${d.getDate()}`;
};

export const clamp = (v, lo, hi) => Math.max(lo, Math.min(hi, v));

// MIN_UNCACHED: below this many *new* tokens in one request, its prefill speed is not a measurement.
// A warm request re-sends a handful of tokens and still pays the fixed cost of the call: 18 uncached
// tokens in 537 ms reads as 33 tok/s, which describes neither the server nor the cache. Measured on
// llama.cpp: 3 198 new tokens in 2 141 ms = 1 494 tok/s, the same request warm = 33.
export const MIN_UNCACHED = 64;

// tps turns one request's counters into tokens per second, or hands back null when the honest answer is
// "there is no number here": a missing timing is missing, and a numerator below `min` is noise.
// Callers render null as an em dash rather than a plausible-looking figure.
export const tps = (tokens, ms, min = 1) => (tokens >= min && ms > 0 ? Math.round(tokens / (ms / 1000)) : null);

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
