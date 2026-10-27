// Getting events out of the server: a websocket carries every agent event, SSE is the fallback if
// websockets are unavailable, and a slow poll is the fallback if even that fails. Only one of the
// three is ever live, and the one that is live is reported in the sidebar.
import { state } from './state.js';
import { setConn } from './ui.js';
import { handleEvent } from './events.js';
import { refresh } from './session.js';

let wsAttempts = 0;
let ws = null;

export function connect() {
  if (state.transport === 'sse' || state.transport === 'poll') return;
  const proto = location.protocol === 'https:' ? 'wss:' : 'ws:';
  try {
    ws = new WebSocket(`${proto}//${location.host}/ws`);
  } catch {
    return fallback();
  }
  ws.onopen = () => {
    wsAttempts = 0;
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
    setConn(false, 'reconnecting…', state.transport);
    wsAttempts++;
    if (wsAttempts >= 3) return fallback();
    setTimeout(connect, Math.min(800 * wsAttempts, 4000));
  };
  ws.onerror = () => {
    try {
      ws.close();
    } catch {
      /* the close handler deals with it */
    }
  };
}

function fallback() {
  if (state.transport === 'sse' || state.transport === 'poll') return startPolling();
  startSSE();
}

function startSSE() {
  if (!window.EventSource || !state.id) return startPolling();
  if (state.es) return;
  const src = new EventSource(`/api/session/${state.id}/events`);
  state.es = src;
  src.onopen = () => {
    setConn(true, 'sse', 'sse');
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
    if (src.readyState === EventSource.CLOSED) startPolling();
  };
}

function startPolling() {
  setConn(true, 'polling', 'poll');
  if (state.poll) return;
  state.poll = setInterval(() => {
    if (state.id) refresh().catch(() => {});
  }, 1500);
}
