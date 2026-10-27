// The one mutable state object, plus the session id accessor every module needs.
//
// Keeping it in a single module (rather than passing a store around) is what lets the transcript,
// the composer and the transport agree on "which session am I showing" without a framework. Nothing
// outside this file may replace the object — mutate its fields, and components re-render from it.

export const state = {
  sessions: [],
  id: null, // current session id
  cfg: null, // last config read from the server (the settings dialog starts from it)
  entries: new Map(), // entry id -> element
  streams: new Map(), // stream id -> {node, body, reasoning, content, reasoningText, tools, args}
  tools: new Map(), // tool call id -> card api
  requests: new Map(), // pending input-request id -> element
  attachments: [], // {name, mimeType, data}
  pending: [],
  status: null,
  seq: 0,
  transport: 'none',
  es: null,
  poll: null,
};

export const sessionId = () => state.id;

export function setSessionId(id) {
  state.id = id;
}
