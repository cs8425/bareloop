// A five-function event bus, used for exactly one thing: letting a leaf module (the gate card's "ask
// again" button) ask for work that lives in a higher module (re-read the session) without
// importing it and creating a cycle. Anything that needs more than this should import directly.

const handlers = new Map(); // event -> Set<fn>

export function on(event, fn) {
  if (!handlers.has(event)) handlers.set(event, new Set());
  handlers.get(event).add(fn);
  return () => handlers.get(event).delete(fn);
}

export function off(event, fn) {
  handlers.get(event)?.delete(fn);
}

// emit calls every handler and swallows nothing: a handler that throws is a bug, and the console is
// where a bug belongs. Handlers are called with the event payload.
export function emit(event, payload) {
  for (const fn of handlers.get(event) || []) {
    try {
      fn(payload);
    } catch (err) {
      console.error(`bareloop: handler for "${event}" failed`, err);
    }
  }
}

// The three events the app actually uses, named here so a typo is a missing constant, not a silent no-op.
export const EV_REFRESH = 'refresh'; // re-read the session from the server
export const EV_SESSIONS = 'sessions'; // re-read the session list
export const EV_SESSION = 'session'; // the session on screen changed: a panel showing the old one must go
