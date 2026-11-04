// Reading a timestamp, which is two questions and not one.
//
//   What does the log say?   UTC, always. `ir.Entry.Ts` is a UTC RFC3339 stamp and nothing here rewrites it.
//   What does the reader see? Whatever they asked for — the browser's own zone by default, or one they pick.
//
// Both are answered from one place because the two must never be confused: a transcript that quietly mixed a
// UTC clock with a local one is how a reader loses an hour of a debugging session. The setting is this
// browser's (localStorage), for the same reason the redraw rate is: it describes the machine in front of the
// page, not the conversation, and two readers of one session are allowed to be on two continents.
//
// The value is `''` (the browser's zone), `UTC`, or an IANA name (`Asia/Taipei`); `Local` is accepted and
// dropped, because the daemon uses that word for the machine bareloopd runs on and the reader's browser is not
// it. An unparseable value falls back to the browser's zone rather than throwing — the transcript has to be
// readable even when the setting is broken.
const KEY = 'bareloop.tz';

let cached = null;

export function tzIsValid(raw) {
  const v = (raw || '').trim();
  if (!v || v === 'Local') return true;
  try {
    new Intl.DateTimeFormat('en-GB', { timeZone: v });
    return true;
  } catch {
    return false;
  }
}

// tzSetting is the zone in force, or '' for "whatever this browser says". It validates on the way out and not
// only on the way in: the storage of a returning visitor was written by an older build, a typo, or a machine
// whose OS no longer ships that zone file, and a page that throws over a preference is a page with no
// transcript in it.
export function tzSetting() {
  if (cached !== null) return cached;
  let raw = '';
  try {
    raw = localStorage.getItem(KEY) || '';
  } catch {
    raw = '';
  }
  cached = tzIsValid(raw) ? raw : '';
  return cached;
}

export function setTzSetting(raw) {
  const v = (raw || '').trim();
  if (!tzIsValid(v)) return false;
  try {
    localStorage.setItem(KEY, v === 'Local' ? '' : v);
  } catch {
    // A profile that refuses localStorage still gets the zone for this page, which is the smaller loss.
  }
  cached = v === 'Local' ? '' : v;
  return true;
}

const zone = () => tzSetting() || undefined;

// Formatters are not free — each one is a locale + zone resolution — and every entry in a transcript asks for
// one, so they are built once per zone. The cache is keyed on the zone it was built for and holds exactly one
// set: changing the setting is a click, not a loop.
let fmtCache = { zone: null, clock: null, full: null, day: null, label: null };

function fmts() {
  const z = zone();
  if (fmtCache.zone !== String(z ?? '')) {
    const opts = {
      clock: { hour: '2-digit', minute: '2-digit', hourCycle: 'h23' },
      // A fixed, locale-independent order for the tooltip: the header of an entry is already a 24-hour clock
      // because a transcript should not change shape when a different laptop opens it (see entryNode), and the
      // full stamp under it is the one place the date and the zone appear.
      full: { year: 'numeric', month: 'short', day: '2-digit', hour: '2-digit', minute: '2-digit', second: '2-digit', hourCycle: 'h23', timeZoneName: 'shortOffset' },
      day: { year: 'numeric', month: '2-digit', day: '2-digit' },
      label: { timeZoneName: 'short' },
    };
    const build = (kind) => {
      try {
        return new Intl.DateTimeFormat('en-GB', { ...opts[kind], ...(z ? { timeZone: z } : {}) });
      } catch {
        return new Intl.DateTimeFormat('en-GB', opts[kind]); // the zone died between the check and here
      }
    };
    fmtCache = { zone: String(z ?? ''), clock: build('clock'), full: build('full'), day: build('day'), label: build('label') };
  }
  return fmtCache;
}

const parts = (f, d) => {
  const p = {};
  for (const x of f.formatToParts(d)) if (x.type !== 'literal') p[x.type] = x.value;
  return p;
};

// fmtTime is the clock in the header of an entry: always HH:MM, 24-hour, in the reader's zone.
export function fmtTime(ts) {
  const d = new Date(ts);
  if (Number.isNaN(d.getTime())) return '';
  const p = parts(fmts().clock, d);
  return p.hour && p.minute ? `${p.hour}:${p.minute}` : '';
}

// fmtDay is a calendar day in the reader's zone, `2026-11-03`. It is a string built from parts rather than
// `toISOString().slice(0,10)` (UTC) or `toLocaleDateString()` (locale-shaped) because it is compared
// neighbour-to-neighbour to decide where a day divider goes: the divider must land where the reader's midnight
// is, and nowhere else.
export function fmtDay(ts) {
  const d = new Date(ts);
  if (Number.isNaN(d.getTime())) return '';
  const p = parts(fmts().day, d);
  return p.year && p.month && p.day ? `${p.year}-${p.month}-${p.day}` : '';
}

// fmtFull is the tooltip: the moment, with the zone named rather than implied. `17:18` under a UTC log and a
// +08 browser is a fact about a machine, and the title is where that machine is identified.
export function fmtFull(ts) {
  const d = new Date(ts);
  if (Number.isNaN(d.getTime())) return '';
  const f = fmts();
  // `timeZoneName: 'shortOffset'` puts "GMT+8" on the end of the string itself, which is the unambiguous half.
  // What an offset cannot say is *which zone the reader asked for* — and that is the exact doubt this tooltip
  // exists to settle (is 20:34 the laptop's time or the time the conversation is about?), so a chosen zone is
  // spelled out by name. A zone nobody chose is not spelled out: naming the OS's own answer would be quoting
  // a fact the reader never picked, and ICU's own short name for it is usually the same "GMT+8" anyway.
  const when = f.full.format(d).replace(/[ ]/g, ' ');
  const z = tzSetting();
  return z && !when.includes(z) ? `${when} · ${z}` : when;
}

// fmtZoneLabel names the zone in force, for the settings row and anywhere a timestamp is quoted without its
// units: "UTC+8", "GMT+8", "UTC".
export function fmtZoneLabel(now = Date.now()) {
  const f = fmts();
  const hit = f.label.formatToParts(new Date(now)).find((x) => x.type === 'timeZoneName')?.value;
  return hit || (tzSetting() ? tzSetting() : 'this browser');
}
