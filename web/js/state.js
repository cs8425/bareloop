// The one mutable state object, plus the session id accessor every module needs.
//
// Keeping it in a single module (rather than passing a store around) is what lets the transcript,
// the composer and the transport agree on "which session am I showing" without a framework. Nothing
// outside this file may replace the object — mutate its fields, and components re-render from it.

export const state = {
  sessions: [],
  id: null, // current session id
  cfg: null, // the config of the session in `id`: filled by refresh(), written by every save, so a whole-config PATCH (settings dialog, tool switches) can never undo the last one
  entries: new Map(), // entry id -> element
  streams: new Map(), // stream id -> {node, body, reasoning, content, reasoningText, tools, args}
  tools: new Map(), // tool call id -> card api
  toolResults: new Map(), // tool call id -> the tool entry that answered it: the only place its ms, its error flag and its id live
  requests: new Map(), // pending input-request id -> element
  attachments: [], // {name, mimeType, data}
  pending: [],
  // What this tab sent that the daemon is holding off the path (a tool call has an answer owed), oldest
  // first. The server's `status.queued` is the count — it is the one thing that also sees another tab's
  // messages and the bolt's — and this is the copy of the text, which only the sender has. It is not a
  // queue: it is a mirror, dropped and re-cut whenever the count says the head has joined the log, and it
  // is emptied when this tab looks at another session because it describes the one it is leaving.
  held: [],
  status: null,
  // The page of the path this tab is showing, as request plus answer. The two live in one object
  // because a refresh has to re-ask exactly what it is showing: {mode, size, pivot, stale} are what
  // this tab asked for and keeps, {count, oldest, newest, hasOlder, hasNewer, total, bytes, max} are
  // what the server says came back. `count` is the only one that answers "how much of the path is on
  // screen" — a page is turn-atomic, so it can arrive smaller than the number asked for.
  // `mode` is 'tip' (the page ends at the newest message) or a parked page around `pivot` ('anchor':
  // centred on the turn holding it, 'before': the page that ends just before it). `stale` counts live
  // events this parked page refused to draw (events.js). `place` is the one exception: a field written
  // just before a refresh and dropped by it — where to put the viewport once the rebuild lands.
  // null until a session has been read, and reset when the tab looks at another session.
  win: null,
  pathIds: [], // ids on the path this tab is showing, root -> tip ([] until a session has loaded; a parked page knows its own page, not the whole conversation)
  seq: 0,
  transport: 'none',
  es: null,
  poll: null,
};

export const sessionId = () => state.id;

export function setSessionId(id) {
  state.id = id;
}

// isParked answers the one question every module asks about the page this tab is showing: is it a page
// that is not the end of the conversation. Live traffic is an answer to "what is happening now", and
// while parked nothing that draws at, or scrolls to, the bottom of the transcript may be believed. This
// predicate lives with the state rather than with the module that pages because three of them ask it —
// the pager, the event handler and the scroller — and none of the three should have to import another to
// answer a question about this object.
export const isParked = () => !!state.win && state.win.mode !== 'tip';
