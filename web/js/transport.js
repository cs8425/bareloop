// Getting events out of the server: a websocket carries every agent event, SSE is the fallback if
// websockets are unavailable, and a slow poll is the fallback if even that fails. Only one of the
// three is treated as live, and the one that is live is reported in the sidebar.
//
// Two rules this module learned the hard way:
//   - Never stop trying the websocket. Giving up after three attempts and staying on a 1.5 second
//     poll for the rest of the tab's life is exactly the wrong answer to the common case — a server
//     restart, or a laptop that woke up: the failure is temporary, the tab is not.
//   - Never let two channels be live at once. When the websocket comes back the fallback is closed
//     first, or every event would be rendered twice.
import { state } from './state.js';
import { setConn, note } from './ui.js';
import { handleEvent } from './events.js';
import { refresh } from './session.js';
import { u } from './base.js';

const MIN_DELAY = 600; // first retry: fast enough that a restarting server costs one blink
const MAX_DELAY = 15000; // and never more than a quarter of a minute between attempts
const FALLBACK_AFTER = 3; // attempts before a second channel opens so the user still sees updates

// The daemon tells each connection where its event counter got to, about every twenty seconds:
// `{"type":"heartbeat","seq":821,"ts":…}`, written by the write pump in ws.go and deliberately not
// published, so it takes no stamp of its own and joins no replay buffer. That one number is why this file
// does not have to guess any more: every event is stamped as it leaves manager.go, every frame carries its
// stamp, and this tab subscribes to everything — so a counter ahead of state.seq is an event that was
// written for this page and never arrived. Nothing is polled to find that out.
const BEAT_MS = 20000; // the daemon's interval, before its own jitter
const BEAT_SLACK = 1.25; // ...and how far early or late it is allowed to be
const BEAT_BEHIND = 2; // consecutive beats behind the counter before the page is re-read
const BEAT_GIVEUP = 3.5; // intervals without a beat before a silent socket is dropped and reopened

let wsAttempts = 0;
let ws = null;
let retry = null; // the pending reconnect, if any
let stopFallback = null; // teardown of the live fallback, or null while the websocket carries events
let beats = 0; // beats heard on this socket; the first one only says where the page stands
let ahead = 0; // consecutive beats that counted fewer frames than the daemon published
let applied = 0; // stamped frames this socket handed to handleEvent, counted from the ruler beat
let appliedBase = 0; // `applied` as of the beat the ruler was laid on
let counterBase = null; // the counter as of that beat, or null until the first beat has been measured
let beatTimer = null; // the "no beat has arrived" alarm, armed once a first beat has been seen

export function connect() {
  if (retry) {
    clearTimeout(retry);
    retry = null;
  }
  // One socket at a time: its handlers close over this module's state, so a second socket opening
  // while the first is still handshaking would have both of them subscribing and refreshing.
  if (ws && (ws.readyState === WebSocket.CONNECTING || ws.readyState === WebSocket.OPEN)) return;

  // The authority is this page's own: a proxy may be sitting in front of the daemon and terminating TLS, and the
  // browser has to dial what it is looking at rather than what the daemon thinks it is called. The path comes
  // from base.js, so a UI mounted under /app/bareloopd/ asks for /app/bareloopd/ws and the proxy strips it back.
  const proto = location.protocol === 'https:' ? 'wss:' : 'ws:';
  try {
    ws = new WebSocket(`${proto}//${location.host}${u('/ws')}`);
  } catch {
    degrade();
    schedule();
    return;
  }
  ws.onopen = () => {
    // What this socket could have missed is the whole question. A socket that opens on the first try of
    // a fresh tab can have missed nothing: main() read the session before calling connect(), and the
    // snapshot it asks for below carries whatever the hub is still replaying from after that read. A
    // socket that is back from a drop is a different story - the replay window is bounded, so the
    // transcript on screen cannot be trusted and the session is re-read. wsAttempts counts the drops and
    // is cleared here, which is why it is read on the line above the clear.
    const backFromADrop = wsAttempts > 0;
    wsAttempts = 0;
    if (stopFallback) {
      stopFallback();
      stopFallback = null;
      note('websocket back up');
    }
    setConn(true, 'websocket', 'ws');
    // The beat bookkeeping belongs to a socket, not to the tab: this one has heard nothing yet, so its
    // first beat is a baseline and its silence is not evidence of anything.
    clearTimeout(beatTimer);
    beatTimer = null;
    beats = 0;
    ahead = 0;
    applied = 0;
    appliedBase = 0;
    counterBase = null;
    ws.send(JSON.stringify({ type: 'subscribe', session: '' }));
    ws.send(JSON.stringify({ type: 'snapshot', session: state.id || '' }));
    if (backFromADrop && state.id) refresh().catch(() => {});
  };
  ws.onmessage = (ev) => {
    let msg;
    try {
      msg = JSON.parse(ev.data);
    } catch {
      return;
    }
    if (msg.type === 'reply' || msg.type === 'reply_error') return; // actions go over REST
    // A heartbeat is transport, not an agent event. It stays out of handleEvent for two reasons: it is not
    // something that happened in a session, and its number is the counter's value — running it through the
    // event path would raise state.seq from the very frame whose job is to notice the page is behind, and
    // the comparison could never fail again.
    if (msg.type === 'heartbeat') return onBeat(msg);
    // The count a beat is compared against is taken here, on the way in, and only for frames that carry a
    // stamp: the two numbers have to be counted in the daemon's own units or they mean different things.
    if (typeof msg.seq === 'number') applied++;
    handleEvent(msg);
  };
  ws.onclose = () => {
    ws = null;
    clearTimeout(beatTimer);
    beatTimer = null;
    wsAttempts++;
    if (wsAttempts >= FALLBACK_AFTER) degrade();
    schedule();
  };
  ws.onerror = () => {
    try {
      ws?.close();
    } catch {
      /* the close handler deals with it */
    }
  };
}

// onBeat is the entire self-healing path: the counter moved and this page never saw the frames that moved
// it, so the session is re-read exactly the way a reconnect re-reads it, with one line in the corner to
// say why the page moved on its own. A page that silently redraws itself is a page nobody trusts after.
//
// What a beat answers is a count, not a watermark, and that difference is the whole design. The first
// version asked whether the counter had run past state.seq, and state.seq is a maximum (events.js:35
// raises it to whichever stamp is largest), so a hole in the middle is invisible: the frame after the hole
// raises the maximum over it and every later beat reads "nothing behind". Measured that way a page sat on a
// finished run — the exact shape this exists to catch — for as long as the tab stayed open.
//
// So the question is arithmetic over counts instead: between the beat the ruler was laid on and this one,
// the daemon published N stamped frames and this socket handed M of them to handleEvent. M below N is a
// hole, whatever the stamps themselves were. Counts are immune to the three things a watermark is not: a
// frame missing in the middle still counts short, so it is seen; a frame that arrives late (the daemon
// stamps before the hub queues and they are not the same lock, so delivery can invert, and a frame stamped
// one moment before a beat can land one moment after it) just shows up in a later window and closes its own
// deficit, which is why a single short beat is not yet evidence; and a page that dialed after the process
// started is behind by nothing, because both numbers are measured from the same beat — the head start that
// used to need a baseline never enters the subtraction.
//
// Then twice in a row, not once, because two separate things are ahead of their own frames: the daemon
// stamps an event before the hub queues it, so a beat can report a number whose frame has not even been
// queued yet, and the stamp and the queue are not the same lock, so two publishers can arrive in the
// opposite order. Both sort themselves out in milliseconds while beats are twenty seconds apart, so the
// second beat is a confirmation rather than a hunch. The trade is stated plainly: a page never redraws
// itself over a frame that was merely late, and a frame that really was lost waits one extra beat — so
// the worst case from loss to repair is two intervals, tens of seconds instead of never.
function onBeat(msg) {
  beats++;
  armBeatAlarm();
  // A frame with no number to compare is read as "nothing to compare", never as "behind": a parse
  // failure is not worth a page redraw.
  if (typeof msg.seq !== 'number') return;
  if (counterBase === null) {
    // The first beat is where the ruler is laid down. From here on both sides of the comparison are
    // deltas, which is what keeps everything this page could not have received out of the arithmetic.
    counterBase = msg.seq;
    appliedBase = applied;
    return;
  }
  if (msg.seq < counterBase) {
    // A counter that went backwards means the daemon restarted underneath a socket that never noticed.
    // Lay the ruler down again rather than subtract into negative numbers for the rest of the tab's life.
    counterBase = msg.seq;
    appliedBase = applied;
    ahead = 0;
    return;
  }
  const expected = msg.seq - counterBase; // frames published since the ruler was laid
  const actual = applied - appliedBase; // frames this socket was handed
  if (expected > actual) ahead++;
  else ahead = 0; // a deficit that closes on its own was a late frame, and a late frame is not a lost one
  if (ahead < BEAT_BEHIND) return;
  ahead = 0;
  const missing = expected - actual;
  // Re-reading moves the ruler with it: the REST reply fills the transcript without ever passing through
  // this socket, so the hole would still count short on the next beat and the page would re-read forever.
  counterBase = msg.seq;
  appliedBase = applied;
  note(`the socket never delivered ${missing} of ${expected} event(s) — re-reading the session`);
  if (state.id) refresh().catch(() => {});
}

// armBeatAlarm watches for the absence of beats. A peer that dies without a close frame is invisible to
// JS — no error, no close, only silence — and silence is also what an idle daemon looks like, so the only
// way to tell them apart is that the daemon promised to speak. When it stops, the connection is half
// open and the one fix available is another one: closing it runs the existing reconnect path, and that
// path already re-reads the session because it knows it was dropped.
//
// Sleeping laptops fall out of this the right way too: the alarm cannot fire while the tab is frozen, so
// it fires the moment the machine wakes, when a half-open socket is exactly what is waiting there.
//
// It is armed only after the first beat has arrived, so a page newer than its daemon keeps behaving
// exactly as it did before. A client that reconnects every 90 seconds because it is waiting for a frame
// an older server never learned to send would be a worse bug than the one this fixes.
function armBeatAlarm() {
  clearTimeout(beatTimer);
  if (!beats) return;
  beatTimer = setTimeout(() => {
    if (!ws || ws.readyState !== WebSocket.OPEN) return; // onclose is already dealing with it
    note('the socket stopped beating — reconnecting');
    try {
      ws.close();
    } catch {
      /* the close handler deals with it */
    }
  }, BEAT_MS * BEAT_SLACK * BEAT_GIVEUP);
}

// schedule waits, then knocks again: exponential, capped, and never the last attempt. The label says
// what is actually live, because "reconnecting…" while a poll is quietly carrying every event would
// describe the attempt rather than the connection.
function schedule() {
  const delay = Math.min(MAX_DELAY, MIN_DELAY * 2 ** wsAttempts);
  const secs = Math.round(delay / 1000);
  if (stopFallback) setConn(true, `${state.transport} fallback · websocket retried in ${secs}s`, state.transport);
  else setConn(false, `reconnecting in ${secs}s`, state.transport);
  retry = setTimeout(connect, delay);
}

// degrade opens the best fallback once. It is a secondary channel: the websocket keeps being retried
// underneath it and takes over again the moment it answers.
function degrade() {
  if (stopFallback) return;
  stopFallback = window.EventSource && state.id ? startSSE() : startPolling();
}

function startSSE() {
  const src = new EventSource(u(`/api/session/${state.id}/events`));
  state.es = src;
  src.onopen = () => {
    setConn(true, 'sse · websocket down', 'sse');
    refresh().catch(() => {});
  };
  src.addEventListener('agent', (ev) => {
    try {
      handleEvent(JSON.parse(ev.data));
    } catch {
      /* a malformed frame is not worth breaking the stream over */
    }
  });
  src.onerror = () => {
    // EventSource reconnects by itself while its readyState is CONNECTING; only once it has given up
    // is it worth dropping to the last channel.
    if (src.readyState === EventSource.CLOSED) stopFallback = startPolling();
  };
  return () => {
    state.es = null;
    src.close();
  };
}

function startPolling() {
  setConn(true, 'polling · websocket down', 'poll');
  state.poll = setInterval(() => {
    if (state.id) refresh().catch(() => {});
  }, 1500);
  return () => {
    clearInterval(state.poll);
    state.poll = null;
  };
}
