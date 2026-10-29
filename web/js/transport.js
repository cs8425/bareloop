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

const MIN_DELAY = 600; // first retry: fast enough that a restarting server costs one blink
const MAX_DELAY = 15000; // and never more than a quarter of a minute between attempts
const FALLBACK_AFTER = 3; // attempts before a second channel opens so the user still sees updates

let wsAttempts = 0;
let ws = null;
let retry = null; // the pending reconnect, if any
let stopFallback = null; // teardown of the live fallback, or null while the websocket carries events

export function connect() {
  if (retry) {
    clearTimeout(retry);
    retry = null;
  }
  // One socket at a time: its handlers close over this module's state, so a second socket opening
  // while the first is still handshaking would have both of them subscribing and refreshing.
  if (ws && (ws.readyState === WebSocket.CONNECTING || ws.readyState === WebSocket.OPEN)) return;

  const proto = location.protocol === 'https:' ? 'wss:' : 'ws:';
  try {
    ws = new WebSocket(`${proto}//${location.host}/ws`);
  } catch {
    degrade();
    schedule();
    return;
  }
  ws.onopen = () => {
    wsAttempts = 0;
    if (stopFallback) {
      stopFallback();
      stopFallback = null;
      note('websocket back up');
    }
    setConn(true, 'websocket', 'ws');
    ws.send(JSON.stringify({ type: 'subscribe', session: '' }));
    ws.send(JSON.stringify({ type: 'snapshot', session: state.id || '' }));
    // A reconnect may have missed events the hub's replay window already dropped, so re-read the
    // session rather than trusting that the transcript on screen is complete.
    if (state.id) refresh().catch(() => {});
  };
  ws.onmessage = (ev) => {
    let msg;
    try {
      msg = JSON.parse(ev.data);
    } catch {
      return;
    }
    if (msg.type === 'reply' || msg.type === 'reply_error') return; // actions go over REST
    handleEvent(msg);
  };
  ws.onclose = () => {
    ws = null;
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
  const src = new EventSource(`/api/session/${state.id}/events`);
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
