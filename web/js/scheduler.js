// The render gate: how often the transcript may redraw a message that is still arriving.
//
// There is no timer in this file, and that is the design, not an omission. An earlier draft armed a
// `setTimeout` per tick, which meant the page kept waking up on its own — and a wake-up with nothing new
// to draw is pure cost, on exactly the machines this is trying to protect. Instead the decision happens
// where the news arrives: `mark()` renders if and only if enough time has passed since the last render,
// and returns immediately otherwise. The text keeps accumulating in the caller's buffer either way, so a
// skipped frame loses no characters — the next render draws everything up to that moment.
//
// Two consequences worth naming, because they are the reason this shape is better:
//
//   * Nothing needs cancelling. The gate belongs to one stream; when the committed entry replaces that
//     stream's node the gate disappears with it. A timer would have outlived it and redrawn something
//     that is no longer on screen.
//
//   * A rate is a ceiling, not a promise. With synchronous renders, `hz = 20` on a machine where one
//     render costs 60 ms is really ~16 Hz — and that is the desired failure: the page thins out instead
//     of queueing work it can no longer afford. The old path had no ceiling at all, so its rate was the
//     provider's chunk rate (measured 80–100 events/s) and its cost grew with the message, which is how
//     a 47 KB reply ended up costing 5 s of main thread and a 120 KB one ended up frozen.
export const DEFAULT_HZ = 20;

// The rungs the adaptive step walks: 50 ms, 100 ms, 200 ms, 1 s, with 20 Hz as the ceiling. Named here
// so the ladder and the ceiling read as one decision instead of two, and so a hand-set value outside the
// ladder is still accepted (`?hz=1000` means "no gate", see resolveHz).
export const LADDER = [20, 10, 5, 1];

// resolveHz answers "what may this tab spend on redrawing?", highest authority first: an explicit URL,
// then the setting the user pinned, then the default. `0` is a real answer, not a missing one — it means
// "do not redraw while it streams, draw once when it is done", which is the right mode for a machine
// that cannot afford 1 Hz either. `hz=1000` (a 1 ms gate) is the documented way to get the old
// render-every-token behaviour back, which makes the two comparable on the same machine.
export function resolveHz(fallback = DEFAULT_HZ) {
  const number = (raw) => {
    if (raw === null || raw === undefined || String(raw).trim() === '') return null;
    const n = Number(raw);
    return Number.isFinite(n) && n >= 0 ? n : null;
  };
  let stored = null;
  try {
    stored = localStorage.getItem('bareloop.renderHz');
  } catch {
    // A profile that refuses localStorage is a normal browser setting, not a reason to stop rendering.
  }
  const fromUrl = typeof location === 'object' ? new URLSearchParams(location.search).get('hz') : null;
  return number(fromUrl) ?? number(stored) ?? number(fallback) ?? DEFAULT_HZ;
}

// makeRenderGate wraps one stream's render function. `render` must be synchronous and must draw whatever
// the caller has accumulated so far — the gate never tells it what changed, only whether now is allowed.
export function makeRenderGate(render, hz = resolveHz()) {
  // Counts, not opinions: the transcript shows nothing from these, but they are what makes "is it
  // actually throttling, and what did it cost" answerable from the console instead of by vibes.
  const stats = { marks: 0, renders: 0, skipped: 0, hidden: 0, ms: 0, max: 0 };
  let last = 0; // 0 = never: the first tokens of a stream should not wait for a window to open
  let dirty = false;

  const interval = () => (hz > 0 ? 1000 / hz : Infinity);

  function paint() {
    const t0 = performance.now();
    last = t0;
    dirty = false;
    try {
      render();
    } finally {
      const dt = performance.now() - t0;
      stats.renders++;
      stats.ms += dt;
      if (dt > stats.max) stats.max = dt;
    }
  }

  return {
    // mark is what a delta says: "there is new text; draw it if you may".
    mark() {
      stats.marks++;
      dirty = true;
      // A hidden tab has nobody reading it, and browsers throttle it anyway — drawing there is the
      // cost with none of the benefit. `flush()` on the way back in makes it correct again.
      if (document.hidden) {
        stats.hidden++;
        return;
      }
      if (performance.now() - last < interval()) {
        stats.skipped++;
        return;
      }
      paint();
    },
    // flush is what `done`, a tab becoming visible, or a user asking says: draw now, no window.
    // It paints only when something is actually unpainted, so a `done` that arrives after the last
    // frame already drew costs nothing.
    flush() {
      if (!dirty) return;
      paint();
    },
    // hz is re-readable so the adaptive step can change its own ceiling; the gate re-reads it per mark.
    setHz(next) {
      hz = Number.isFinite(next) && next >= 0 ? next : DEFAULT_HZ;
    },
    get hz() {
      return hz;
    },
    get intervalMs() {
      return interval() === Infinity ? 0 : interval();
    },
    get dirty() {
      return dirty;
    },
    stats,
  };
}
