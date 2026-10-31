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

// The rungs the user can pick, and the order the picker shows them in: 50 ms, 100 ms, 200 ms, 1 s, and
// then 0. `0` is not "off" and not a missing value — it is the honest bottom of the ladder: no drawing at
// all while the reply arrives, one draw when it is over. It belongs in the ladder because "my machine
// cannot afford even 1 Hz" is a real complaint, and the alternative (a full parse every second) is worse
// for it, not better. `?mode=plain` remains the *readable* version of the same bargain: text as it
// arrives, markdown only at the end.
export const LADDER = [20, 10, 5, 1, 0];

// AUTO is a rate the gate works out for itself instead of one the person pinned. It is a value the setting
// can hold (`localStorage['bareloop.renderHz'] === 'auto'`, or `?hz=auto`), not a checkbox hiding behind
// the number, because the settings row has to keep showing what is actually in force.
export const AUTO = 'auto';

// AUTO_INITIAL_HZ is where auto starts. Not a computed number: until a frame has been timed there is
// nothing to divide by, so it starts on the rung this project already ships as the default.
export const AUTO_INITIAL_HZ = DEFAULT_HZ;

// The controller in the shape `_state/render-throttle-plan.md:91-94` specified: budget = 1000/rung, three
// consecutive expensive frames step a rung down, twenty consecutive cheap ones step back up, and a frame
// that is neither breaks both runs, because that is what "consecutive" says.
const DOWN_RUN = 3;
const UP_RUN = 20;
const DOWN_AT = 0.6;
const UP_AT = 0.25;

// describeHz is how the UI says a rung: a rate when there is one, a promise when there is not. A number
// alone would not tell someone why their screen is showing nothing.
export const describeHz = (hz) =>
  hz === AUTO ? 'auto, one rung at a time' : hz > 0 ? `${hz} Hz` : 'once, when it is over';

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
  const word = (raw) => (String(raw).trim().toLowerCase() === AUTO ? AUTO : null);
  const fromUrl = typeof location === 'object' ? new URLSearchParams(location.search).get('hz') : null;
  // Why the two passes: `auto` is a word and a number cannot be a word, so each source is asked once for
  // the word and once for a rate, highest authority first. A row that read the number first would let a
  // stale `20` outlive the `auto` someone typed into the same box.
  return word(fromUrl) ?? word(stored) ?? number(fromUrl) ?? number(stored) ?? number(fallback) ?? DEFAULT_HZ;
}

// The three things a stream can do with markdown while it is still arriving. They differ in what they
// hand the parser per frame, which is the only cost that matters while text is streaming in:
//
//   tail   freeze the blocks that can never change, parse only the part after them. The default: it is
//          the difference between parsing the reply once per frame and parsing it once per token.
//   plain  no markdown at all while it streams (text only), one parse when it is over. For a machine that
//          cannot afford even 1 Hz of parsing; the reply is unreadable-styled but never frozen.
//   full   parse the whole accumulated reply every frame. This is what shipped before the render gate —
//          kept reachable because a benchmark you can turn off is evidence, and one you cannot is a rumour.
export const MODES = ['tail', 'plain', 'full'];

// resolveStreamMode resolves the mode the same way resolveHz resolves the rate: URL, then the pinned
// setting, then the default. `?mode=full` on a reply is how you compare the two paths side by side.
export function resolveStreamMode(fallback = 'tail') {
  const pick = (raw) => {
    const v = raw === null || raw === undefined ? '' : String(raw).trim();
    return MODES.includes(v) ? v : null;
  };
  let stored = null;
  try {
    stored = localStorage.getItem('bareloop.streamMode');
  } catch {
    // Same as resolveHz: a profile without localStorage still gets a working default.
  }
  const fromUrl = typeof location === 'object' ? new URLSearchParams(location.search).get('mode') : null;
  return pick(fromUrl) ?? pick(stored) ?? pick(fallback) ?? 'tail';
}

// makeRenderGate wraps one stream's render function. `render` must be synchronous and must draw whatever
// the caller has accumulated so far — the gate never tells it what changed, only whether now is allowed.
export function makeRenderGate(render, hz = resolveHz()) {
  // Counts, not opinions: the transcript shows nothing from these, but they are what makes "is it
  // actually throttling, and what did it cost" answerable from the console instead of by vibes.
  const stats = { marks: 0, renders: 0, skipped: 0, hidden: 0, ms: 0, max: 0, firstAt: 0, lastAt: 0 };
  let last = 0; // 0 = never: the first tokens of a stream should not wait for a window to open
  let dirty = false;

  // AUTO lives in here rather than in a wrapper, because the ceiling is re-read per mark (that is how a
  // mid-stream `setHz` already works) and a second object would be a second place for that number to disagree.
  let auto = hz === AUTO;
  let pinned = auto ? AUTO_INITIAL_HZ : hz;
  let target = AUTO_INITIAL_HZ; // only read while `auto` is on
  let over = 0;
  let under = 0;
  let markAt = 0; // when this delta arrived, for `settled()` to time the whole of it
  let cost = 0; // the last sample the controller saw, so the header can say what it is doing
  let settleSeen = false; // has anyone ever called `settled()`? decides which ruler is in use
  let paintedThisMark = false; // a delta that drew nothing has no cost worth reporting

  const ceiling = () => (auto ? target : pinned);
  const interval = () => (ceiling() > 0 ? 1000 / ceiling() : Infinity);

  // The ladder move, up and down. Reading `LADDER` instead of halving the rate keeps "auto picked 5 Hz"
  // meaning the same thing to a reader as "you picked 5 Hz".
  function moveDown() {
    const next = LADDER.filter((r) => r < ceiling())[0];
    // `undefined` = nothing below: 0 is the bottom, and the bottom is a real place to live (nothing is
    // drawn until the reply ends), not a value to creep away from.
    if (next === undefined) return;
    target = next;
    over = 0;
    under = 0;
  }

  function moveUp() {
    const next = LADDER.filter((r) => r > ceiling())[0];
    // `undefined` = already the top rung: auto never climbs above the fastest thing a person is offered.
    if (next === undefined) return;
    target = next;
    over = 0;
    under = 0;
  }

  // feed judges one frame against the budget that frame's own rung promised, which is the plan's formula
  // (1000/hz) and not a constant, so 40 ms is a failure at 20 Hz and a success at 5 Hz.
  function feed(sample) {
    cost = sample;
    // Why the early return: a pinned rate is an instruction, not a starting point (:96 "使用者釘死時跳過自適應").
    if (!auto) return;
    const budget = 1000 / ceiling(); // Infinity at 0 Hz: nothing is drawn there, so nothing is ever fed and the bottom rung holds itself
    if (sample > DOWN_AT * budget) {
      over++;
      under = 0;
      if (over >= DOWN_RUN) moveDown();
    } else if (sample < UP_AT * budget) {
      under++;
      over = 0;
      if (under >= UP_RUN) moveUp();
    } else {
      over = 0;
      under = 0;
    }
  }

  function paint() {
    const t0 = performance.now();
    last = t0;
    dirty = false;
    if (!stats.firstAt) stats.firstAt = t0;
    stats.lastAt = t0;
    try {
      render();
    } finally {
      const dt = performance.now() - t0;
      stats.renders++;
      stats.ms += dt;
      if (dt > stats.max) stats.max = dt;
      paintedThisMark = true;
      // Why the fallback: a caller that never adopted `settled()` (the bench page is one) must still get an
      // auto that moves. But the closure number is the wrong ruler — `_state/render-throttle-plan.md:510-514`
      // measures it 150-220x below the real cost of a delta — so `auto.kind` says which ruler is in use
      // instead of letting the two be mistaken for each other.
      if (!settleSeen) feed(dt);
    }
  }

  return {
    // mark is what a delta says: "there is new text; draw it if you may".
    mark() {
      stats.marks++;
      // Why stamped here: the expensive part of a frame is the first layout read *after* `render()` returns
      // (the follow-scroll), so `settled()` has to measure from the arrival of the delta, not from the paint.
      markAt = performance.now();
      paintedThisMark = false;
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
    // settled is what the last line of `streamDelta()` says once the follow-scroll is done: "this delta is
    // over, that is what it cost". Only a delta that actually drew has a cost worth reporting — a skipped
    // frame is one mark and a comparison, and feeding its ~0 ms would be a sample that lies the other way.
    settled() {
      if (!paintedThisMark) return;
      settleSeen = true;
      paintedThisMark = false;
      feed(performance.now() - markAt);
    },
    // hz is re-readable so the adaptive step can change its own ceiling; the gate re-reads it per mark.
    setHz(next) {
      // Why the word case: the settings row hands back `auto`, and re-entering it starts from the rung the
      // ladder trusts rather than from wherever the pinned number happened to be sitting.
      if (next === AUTO) {
        auto = true;
        target = AUTO_INITIAL_HZ;
        over = 0;
        under = 0;
        return;
      }
      auto = false;
      pinned = Number.isFinite(next) && next >= 0 ? next : DEFAULT_HZ;
    },
    get hz() {
      return ceiling();
    },
    // What auto is doing, for the streaming header and the console: the rung it picked, the last sample,
    // and which ruler produced it. `null` when the rate is pinned, so a reader can tell the two apart.
    get auto() {
      if (!auto) return null;
      return {
        initial: AUTO_INITIAL_HZ,
        target,
        over,
        under,
        cost: +cost.toFixed(2),
        budget: target > 0 ? +(1000 / target).toFixed(1) : Infinity,
        kind: settleSeen ? 'delta' : 'closure',
      };
    },
    get intervalMs() {
      return interval() === Infinity ? 0 : interval();
    },
    // measuredHz is the rate that actually happened, which is the number worth showing: a ceiling of
    // 20 Hz on a slow machine is really ~16 Hz, and the user should see the truth rather than their own
    // setting echoed back. Two paints is the minimum that makes a rate, so a stream that painted once
    // reports 0 (= nothing measurable yet) rather than inventing a number. It is a rate since the stream
    // began, including the part before a mid-stream setHz() — deliberately not split, because the honest
    // alternative (per-phase counters) would spend a whole set of statistics to make one label fresher.
    get measuredHz() {
      const span = (stats.lastAt - stats.firstAt) / 1000;
      return stats.renders > 1 && span > 0.2 ? +((stats.renders - 1) / span).toFixed(1) : 0;
    },
    get dirty() {
      return dirty;
    },
    stats,
  };
}
