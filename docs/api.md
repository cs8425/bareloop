# The HTTP and websocket API

The complete surface: every endpoint, every parameter, every event frame. The reasoning for
why the API is shaped this way lives in [design.md](design.md); what lives here is the contract
a client can rely on.

`bareloopd` serves everything from one port: the JSON API under `/api/`, the event transports
(`/ws`, `/api/session/{id}/events`), and the web UI at `/`. The UI is not privileged — every
button it presses is one of the calls below, and `curl` can do everything the page can.

**Actions are REST, always.** The websocket carries events out and three transport verbs in
(`ping`, `subscribe`, `snapshot`); nothing a session does is asked for over it. One action, one
decoder — a second way to run a session is how a frame ends up losing a field the first one kept.
Anything else arriving on the socket is answered with an error that says where to go instead.

## Conventions

- **Errors** are one envelope: a non-2xx status and a body of `{"error": "a sentence"}`. The
  sentence names the caller's next move, not an internal state — this server has no error codes.
  `404` is an unknown session id, `409` is creating over an existing directory, `400` is
  everything the request got wrong (including an id that is not on the path, a run in flight
  where the route refuses one, and every refused combination below), `405` is a method the
  route does not speak, `500` is a log that cannot be written.
- **Request bodies** are JSON; unknown fields are tolerated, so a client may send more than the
  server currently reads. Absent is not `false` and not `""`: every optional field is decided by
  its absence.
- **Query parameters**: an empty value reads as absent (`?before=&limit=20` is a client that
  always serialises its window, asking for no cut), and a name this API does not read is ignored —
  unknown parameters are the caller's freedom. Boolean query parameters are picky: `1`/`true` is
  on, `0`/`false`/absent is off, anything else is a 400 rather than a coin flip.
- **The API key is never echoed back.** Every answer that carries a `config` (transcript,
  `?config=1`, `?usage=1`) has `llm.apiKey` blanked.
- **Touching `{id}` opens the session** as a side effect of looking at it — reading the first
  page connects its MCP servers and parses its log. `POST .../unload` is how that side effect is
  given back; `.../flags` and `.../unload` themselves never open anything.
- **Timestamps** are UTC, RFC 3339. **Entry ids** are per-session and sortable; the log assigns
  them, nobody passes them in.

## The surface in one table

| method | path | what it is |
|---|---|---|
| GET | `/api/health` | liveness: `{ok, time}` |
| GET | `/api/sessions` | every session as a list row (below); answered from the index, opens nothing |
| POST | `/api/sessions` | create a session; body is a config, `id` required |
| POST | `/api/sessions/import` | create a session from a transcript file — bareloop's own export or a llama-ui one |
| GET | `/api/session/{id}` | the transcript endpoint: config, status, a page of the path — or `?config=1` / `?usage=1` |
| PATCH | `/api/session/{id}` | update the config (refused while a run is active) |
| DELETE | `/api/session/{id}` | close and delete the session and its directory |
| POST | `/api/session/{id}/unload` | close the session, touch nothing on disk |
| POST | `/api/session/{id}/fork` | copy the session — one path or the whole tree — into a new session |
| GET | `/api/session/{id}/export` | the transcript as one downloadable file, compressed or not |
| POST | `/api/session/{id}/flags` | pin / archive a session without opening it |
| GET | `/api/session/{id}/inspect` | what the next request would cost, per row |
| GET | `/api/session/{id}/payload` | the exact body of the next request |
| GET | `/api/session/{id}/tools` | the tools drawer view |
| POST | `/api/session/{id}/tools` | set one server's tool policy |
| GET | `/api/session/{id}/turns` | a ruler over the path: one row per turn, no content |
| POST | `/api/session/{id}/send` | send a message now |
| POST | `/api/session/{id}/interrupt` | abort what is in flight, then send |
| POST | `/api/session/{id}/stop` | abort what is in flight |
| POST | `/api/session/{id}/resume` | continue from the current tip |
| POST | `/api/session/{id}/compact` | compact now |
| POST | `/api/session/{id}/gate` | answer a gate card |
| POST | `/api/session/{id}/elicit` | answer a tool's elicitation |
| POST | `/api/session/{id}/entry/patch` | rewrite one entry's content in place |
| POST | `/api/session/{id}/entry/delete` | delete an entry |
| POST | `/api/session/{id}/tip` | move the tip (time travel) |
| POST | `/api/session/{id}/reload` | re-read the log from disk |
| GET | `/api/session/{id}/events` | the event stream as SSE |
| GET | `/ws` | the event stream as a websocket |
| GET | `/debug/pprof/…` | only with the `-pprof same` mount |
| GET | `/testdata/…` | only with a testdata directory named |
| GET | `/` | the web UI (single-page app, SPA fallback) |

## Reads

### `GET /api/sessions`

`{sessions: [row]}`, one row per session directory, open or not:

`id`, `name`, `model`, `phase` (`idle|running|compacting`, empty when closed), `closed`,
`path` (entries on the path), `log` (records in the log), `bytes` (what the log costs the disk),
`skipped` (records this build can read but not act on), `tip`, `pinned`, `archived`, `lastTs`.

The numbers are answered from the index — listing sessions reads no transcript — and are omitted
rather than zeroed when there is no index to ask, because a zero is a claim about an empty
session and a missing figure is a claim about nothing. `lastTs` is the newest timestamp any entry
carries — the log's newest entry, not the entry the path stands on, so rewinding a path never
makes it look fresh.

### `POST /api/sessions`

Body: a session config (the same shape `GET /api/session/{id}` answers with). `id` is required,
is trimmed, and becomes the directory name — `400` without it or with a name that cannot be a
directory, `409` if the directory exists (the directory is the identity, even when no log has
been written yet). Everything the body does not carry comes from the daemon's defaults. Refused
before anything is written — pricing, structured-content mode and ceiling, proxy and trust
policy, builtin tool names — so a typo is heard about now, not as a failed request an hour later.
Answer: `{id, status}`.

### `GET /api/session/{id}` — the transcript, paged

Answers with the **path** — the entries that would be sent, oldest first — and nothing else.
An entry on a branch nobody is standing on is not part of any path, so it stays in the log and
in `/inspect` instead of crossing the wire to be thrown away.

The shape:

```json
{
  "config":  { "...": "the session config, API key blanked" },
  "status":  { "...": "the status line (see Status below)" },
  "entries": [ { "...": "ir entries, path order" } ],
  "pending": ["ids of messages on the path but not yet sent"],
  "window":  { "...": "always present, even for a whole path" }
}
```

**No request shape buys the whole path, and that is the contract rather than a default to
override: the largest transcript this endpoint hands over is one page.** Silence gets a page,
and so does a place named without a size; no name the endpoint reads asks for the whole path —
one once did, and it was taken out back before it had a user to mislead. An endpoint that ships
images inline must not answer to the size of the session: one session's unparameterised read
measured 73 MB once, and the request that made it was "no query string".

| parameter | what it asks for |
|---|---|
| *nothing* | the newest page (40 entries — silence buys a page, not the transcript) |
| `?limit=40` | the newest page of at most 40 entries; 200 is the ceiling whatever you ask for, and `window.clamped` says when it was used |
| `?limit=40&before=<id>` | the page before that one — `before` is exclusive and is normally the `oldestId` just received; without a `limit` it is the default page older than there, not "everything older" |
| `?limit=40&after=<id>` | the page after that one — `after` is exclusive too and is normally the `newestId` just received: what comes back begins on the next turn, so nothing a reader holds is handed to it twice; without a `limit`, the same default as `before` |
| `?anchor=<id>&limit=40` | a page centred on one entry, for a jump that lands far from both ends; an anchor is a place in a page, so it is asked for with a size |
| `?config=1` | config and status alone, without walking the path — the shape a settings dialog reads |
| `?usage=1` | the whole path as a skeleton — see below |

Past the tip `after` has nothing left to give: an empty page comes back with `hasNewer` false.
An id that is not on the path is a 400 either way — answering such a request with the whole path
would be guessing at it.

**A page is a run of whole turns**: it begins on a user message or on a compaction summary, so a
tool call never sits on one page with its result on the next, and walking `before` back from the
newest page reproduces the path with no gap and no overlap. The one exception is a single turn
larger than the page it was asked into: it comes back whole and `truncated` is true, because two
pages holding half a tool call each are two pages neither can draw. Walking `after` forward does
the same thing in the other direction, which is what lets a reader let go of what it has scrolled
past and ask for it again later.

**Refused with 400**: any two of `before`, `after` and `anchor` together; either alone-shape
(`config`, `usage`) with any window parameter; a `limit` that is not a positive integer; an
`anchor` with no `limit`; an id that is not on the path.

**`window`** is always there on a transcript answer, so a client has one code path for a page and
for a whole path:

| field | meaning |
|---|---|
| `oldestId`, `newestId` | the ids the page begins and ends on (empty for an empty page) |
| `hasOlder`, `hasNewer` | whether the path continues on that side |
| `count`, `total` | entries on this page; entries on the whole path |
| `bytes` | the encoded size of the entries themselves |
| `limit` | the page size in force |
| `maxLimit` | the ceiling: 200 (`ir.MaxPageEntries`) |
| `clamped` | the ask was over the ceiling, and this is the answer at the ceiling |
| `truncated` | a single turn larger than the page came back whole anyway |

**`?config=1`** answers `{config, status}` and touches nothing else — no path walk, no entries,
API key blanked. It is what a settings dialog reads.

**`?usage=1`** answers `{config, status, entries, count}` where every entry is a skeleton row:
`id`, `parentId`, `kind`, `role`, `usage`, `finishReason`, `ms`, `ts`. Content, reasoning, tool
calls and images are absent **by construction, not by flag** — the server never assembles them
for this answer. It is the shape of a bill, the one whole-path answer the endpoint keeps, and
kept because it is bounded by the path's length rather than by what was said in it.

**Entry shape** (the entries inside a transcript page — what a `?usage=1` row strips away):
`id`, `parentId`, `kind` (`user|assistant|tool|compaction`), `role`, `content`, `reasoning`,
`parts[]` (`{type, text, mimeType, data, name}` — images travel as base64 inside one message),
`toolCalls[]` (`{id, name, arguments, server}`), `toolCallId`, `tool`, `server`, `isError`,
`structuredContent`, `interrupted`, `model`, `usage` (`{prompt, cache, completion, promptMs,
completionMs, ...}`), `finishReason`, and a compaction's summary fields (`boundaryId`,
`replaced`, `summarySize`, ...). Absent means the entry has none.

### `GET /api/session/{id}/inspect`

What the next request would cost, per row and not per guess: the exact system head
(`system`), per-message token rows (`messages[]`: `id`, `kind`, `role`, `tokens`, `real`
(measured vs estimated), `bytes`, `head`, `inKept`), `nCtx`, `contextUsed`/`contextKnown`,
`budget`, `summaryCap`, `boundaryId`/`boundaryBroken`, `totalBytes`, `syntheticToolResults`,
the compaction `plan` and why it was or was not cut (`planReason`), and the tool names a
request would carry. Sizes are estimates (`estimated: true`) until a real usage has anchored
them. Nothing crosses the wire that the request itself would not carry — content lives on the
next call.

### `GET /api/session/{id}/payload`

The exact body of the next request: `{system, messages, tools}`. Where `/inspect` sizes the
prompt, this shows it; nothing about the prompt is hidden behind a byte count. This is the
debugging endpoint — it is what "what did the model actually see" reads.

### `GET /api/session/{id}/export`

One transcript file in the response body: JSONL, `application/x-ndjson`, sent as an
attachment whose name follows the llama-ui habit
(`2026-11-07_15-04_conv_34j9lyjwm6_shopping_list.jsonl`). Line 1 is a header —
`{harness: "bareloop", format: "bareloop/1", tip, scope, session: {id, name, config}}`,
the config stripped of the apiKey and custom headers for the same reason the transcript
never echoes them — and every later line is one entry in exactly the shape the transcript
endpoint answers it: the export adds no format of its own, so a file can be `jq`-ed with
whatever one already knows about entries. Ids are kept, not reissued: the file is the
session's log, and re-importing what one exported answers the same tree, byte for byte.

- `?path=` picks what travels, and the header records the choice as `scope`:
  - unset — the whole tree, abandoned branches included: a clone.
  - `tip`, `head`, `active`, `path`, or any entry id — the single line to that entry; a path export's tip is the cut.
  - `ctx` — only what the next request would carry: the active summary plus everything
    from its boundary on (the entries it did not replace, which sit on both sides of it
    on the path), with the window's head entry rooted. The window is computed by the
    same function the assembler reads, so importing a `ctx` file answers `GET payload`
    byte for byte like the source — a thousand replaced entries stay home. Without an
    active summary (or with a broken one, which fails open) the model reads the whole
    path, and the window is the path.
- `?codec=zstd|gzip|none` picks the encoding, zstd by default. The compressed bytes ride
  as the response body with **no `Content-Encoding`** — the file named `.zst` is the file
  the browser stores, whatever proxy sits in between. There is no size ceiling: this is
  the controlled dump, not the list API.

### `POST /api/sessions/import`

The inverse: the request body is the file, compressed or not — the first bytes and the
first line decide the dialect (bareloop's own, or llama-ui's `harness: "llama.app"`), not
a query parameter or a file name. `?id=` is required and the caller's, same rule as
create and fork; `?name=` overrides the name the file remembers. A native file seeds the
config (secrets stay stripped); a webui file's system root becomes the session's system
prompt, its message tree lands entry for entry, its `currNode` becomes the tip. Two doors
stand on size: the request body caps at 50 MB before a byte is parsed, and the decompressed
stream caps at 512 MB as it unfolds — a bomb never exists in full anywhere. The whole file
lands in one store transaction: anything rejected (a duplicate id, a parent the file never
carries, an unknown dialect) answers `400` and creates no session, `409` for a taken id,
`413` over either cap. Imports always create new sessions; merging into an existing
history is not a thing this does.

### `GET /api/session/{id}/tools` and `POST /api/session/{id}/tools`

The drawer view (`ToolsView`): every server with `name`, `transport`, `connected`, `disabled`,
`builtin`, `prefix`/`configuredPrefix`, `known`/`enabled`, and every tool it offers — `name`
(the model-visible one), `rawName` (what `tools/list` said), `renamedFrom` (a collision rename),
`title`, `description`, `schema` (the parameters as the model sees them), `enabled`,
`readOnly`, `estTokens` (a bytes/4 estimate of what the definition costs per request), `uses`,
`lastUsed`. Plus `visible`/`total`, `pendingChanges`, `appliesTo` (`"next-run"` — the tool block
is rendered in front of the history, so replacing it mid-run would invalidate the KV prefix).
The GET answers in any phase and never asks an MCP server anything new.

`POST` body: `{server, disabledTools[], toolPrefix}` — one server's switches and/or its prefix.
Absent fields are not changes, which is what lets a drawer send one small object instead of the
whole config; `{server: "x"}` alone reads that server back. It works while the session runs,
because the change waits for the end of the run instead of invalidating the payload in flight —
the whole-config `PATCH` still refuses mid-run on purpose (it can change the model, which is a
different promise). A change lands as a `tools` event.

### `GET /api/session/{id}/turns`

The same path at the resolution of a ruler instead of a page: `{turns, count, total, hasOlder,
limit, maxLimit, clamped}` with one row per turn — `id` (the entry to jump to: the first entry
of the turn, and therefore a legal page boundary by construction), `kind` (`user`, or
`compaction` for a summary), `ts`, and `snippet` (the first line, capped at 120 runes with an
ellipsis where it was cut; an attachment reports its name) — and no content at all. 500 rows
per answer (`maxLimit`); asking for more of a longer session is not an error: the newest 500
come back, `clamped` is true and `hasOlder` says the path goes on before the first row.
`?limit=` takes fewer, from the newest end. `before`, `after` and `anchor` are 400s here — a
turn list that skipped a stretch of the path without saying so is worse than one that refuses.

## Writes

Every action route answers `{"ok": true}` plus whatever the caller can repaint without asking
again; every failure is a 400 sentence naming the next move (wrong phase, unknown id, empty
message). A message that arrives while the path is not free is **held in memory**, not refused —
[the three moments](../README.md#three-moments-for-a-message) are the contract behind `send`.

| route | body | answers | notes |
|---|---|---|---|
| `POST .../send` | `{text, images[], files[], label}` | `{ok, status, pending}` | `send` now: appended, or held until the path is free. Whitespace alone is nothing (400); an image counts as a message |
| `POST .../interrupt` | same shape, **required** | `{ok}` | abort the generation or the parked wait, then send this. Aborting without saying anything is what `/stop` is for. Refused while a manual summary runs |
| `POST .../stop` | — | `{ok}` | a run keeps what it produced; a parked wait is ended; a manual summary is abandoned (it writes nothing until it finishes, so stopping one loses nothing) |
| `POST .../resume` | — | `{ok, status}` | continue from the tip without appending; the path must end on a user message, a tool result, or a compaction summary |
| `POST .../compact` | `{instruction}` (optional steer) | the compaction result | the summary belongs to the session, not to the request: disconnecting does not end it, `/stop` does |
| `POST .../gate` | `{id, answer}` | `{ok}` | `answer` is `accept`, `retry` or `skip` on an oversized-summary gate |
| `POST .../elicit` | `{id, behavior, content}` | `{ok}` | `behavior` defaults to `accept`; `content` is the elicited form values |
| `POST .../entry/patch` | `{id, content}` | `{ok}` | rewrite in place: every path through the entry changes, and it does not branch |
| `POST .../entry/delete` | `{id}` | `{ok}` | delete a leaf, or make an interior entry synthetic |
| `POST .../tip` | `{id}` | `{ok, status}` | move the tip — time travel; the log keeps everything, the path changes |
| `POST .../reload` | — | `{ok, status}` | re-read the log from disk (another writer edited the session) |
| `POST .../unload` | — | `{ok, unloaded, session}` | gives back the RAM without touching the directory. A run or summary in flight is refused with a 400 naming the phase — `/stop` first. `unloaded: false` is the honest answer for a session this process was not holding: it is shut, which is what was asked for. What only memory had goes — a held message, the in-memory half of a parked wait — the same bill a restart pays |
| `POST .../fork` | `{id, at?, full?, name?}` | `{id, entries}` | copy the session into a new one whose id the caller picks (required; the daemon invents no directory names, as in create). Default copies the path from the root to `at` — the tip when `at` is empty, any entry on the tree otherwise — as a straight line, leaving abandoned siblings and rewound branches behind; `full:true` copies every branch and the tip, a snapshot at this moment that later turns in the source do not follow. Either way every entry id is reissued and the parent edges, compaction boundaries included, follow the map — the two histories never quote each other's ids. Config copies with the transcript (`name` defaults to "«source» (fork)"); the response never carries an apiKey. `409` for a taken id, `400` for an `at` that is not in the session |
| `POST /api/sessions/import` | the transcript file itself | `{id, format, entries, status}` | create a session from a file — see the import section under Reads; one transaction or nothing |
| `POST .../flags` | `{pinned, archived}`, either pointer | `{ok, session}` | pin/archive **without opening** the session (pinning is four bytes of config.json, opening is reading a transcript). Both-true is a change of mind, and the flag just asked for wins. The answer carries the row this produced, `closed: true` included |
| `PATCH /api/session/{id}` | a config (partial) | `{ok, status}` | a field the body does not carry is kept, not cleared; refused while a run is active. The name is a config field, so this is also how a session is renamed |
| `DELETE /api/session/{id}` | — | `{ok}` | close and delete the directory |

## Events

One object shape on both transports — websocket frames and SSE `data:` lines carry the same
JSON:

```json
{"type": "...", "session": "...", "seq": 42, "ts": "...", "...payload"}
```

`seq` is the daemon's event counter, shared across sessions: a page that learns its socket died
can compare the last `seq` it saw against the counter (`ping`, or the `heartbeat` frame) and
know "nothing happened" from "something happened that I never received".

| field | carried by | contents |
|---|---|---|
| `entry` | `entry` | the full committed entry |
| `streamId` | `entry` | the id the reply's `delta` frames came on — only a streamed entry carries it |
| `delta` | `delta` | `{streamId, content, reasoning, toolCall{id,name,arguments,index}, done, error}` |
| `state` | `state` | the full status line |
| `pending` | `pending` | the ids on the path but not yet sent |
| `run` | `tool_call`, `tool_result` | `{id, callId, server, tool, arguments, status: running|ok|error, result, structuredContent, images, ms, error, entryId, rounds}` |
| `gate` | `request` | `{id, summarySize, summaryCap, attempts, preview}` |
| `elicitation` | `request` | `{id, server, tool, message, mode, schema, round}` |
| `info` | `request_done` (the closed request's id), `removed` (the entry id), `hello`, `log`, `info`, `error` | a string |
| `level` | `log`, `info`, `error` | severity |
| `data` | `tools` (the tools view), `compaction` (stage progress or the result), others | any JSON |

| type | when it fires |
|---|---|
| `hello` | the connection opened (first frame, always) |
| `entry` | an entry committed to the path |
| `delta` | a streaming fragment of the running assistant message |
| `tool_call` | a tool call is starting |
| `tool_result` | a tool call finished (or the path refused its result — see below) |
| `state` | the status line changed |
| `pending` | the pending-id set changed |
| `compaction` | a compaction round's progress (`{stage, attempt}`) or its finish (the result) |
| `request` | the session needs a human: a gate card or an elicitation |
| `request_done` | that request was answered or cancelled (`info` names it) |
| `log` / `info` / `error` | diagnostics for the page to show |
| `removed` | an entry was deleted (`info` names it) |
| `tools` | the published tool set changed; the body is the same view `GET .../tools` returns |

Not events, but on the same socket: `heartbeat` frames (below) and command answers —
`{type: "reply", req, data}` / `{type: "reply_error", req, error}`.

**Stream lifecycle.** A reply is drawn from `delta` frames keyed by `streamId`; the `entry`
frame for that reply arrives **before** the closing `delta` and carries the same `streamId`, so
the client finishes *that* node in place — id chips and token counts arriving, the streaming
marker going — instead of drawing a second node for a message it watched being written. Entries
that were never streamed omit the key. A closing `delta` says `done` and normally nothing else;
the one case where that is a lie is a reply storage refused, and there the closing frame carries
`error` beside the `done` — a live buffer is keyed by a stream id and an `error` event has none.
A tool result the path refuses is the same case on the frame that ends its card: the
`tool_result` carries the refusal as its error, because a card left spinning reads as a call
still running. What a page does with both: the refused text stays on screen marked as not saved,
because what it has on screen is then the only copy anywhere.

### `GET /api/session/{id}/events` — SSE

`text/event-stream`, one `event: agent` frame per event, a `: ping` comment every 25 s to keep
proxies honest. The id in the path selects the replayed session; an empty path segment subscribes
to everything. On connect, the hub's replay buffer for that session is written out first, so a
page that reloads mid-run catches up on what it missed instead of showing a blank screen.

### `GET /ws` — websocket (the primary transport)

Text frames, JSON, read limit switched off (images travel as base64 inside one message).
`?session=a,b` at connect time chooses the subscription; empty means all sessions. A `subscribe`
command can change it live. Events for other sessions are never queued for a connection.

Commands in (the three transport verbs, and nothing else):

| command | answers | what it does |
|---|---|---|
| `ping` | `{pong: <time>}` | a control ping; the `req` you send is echoed |
| `subscribe` | `{ok}` | `{session: "a,b"}`; empty means all |
| `snapshot` | `{ok}` | replays the hub's buffer for `{session}` |

Anything else — including anything that looks like an action — is answered with a `reply_error`
pointing at REST. A `req` on any command comes back on the reply; frames without one are matched
by order.

Out, after `hello`: events, replies, and a **`heartbeat`** frame every ~20 s (±25% jitter):
`{type: "heartbeat", seq, ts}`. It is written straight by the same pump that writes events and
carries the counter's value at the moment of writing — a page can tell "nothing happened" from
"something happened that I never received". It is not an event: no stamp of its own, no replay
buffer, and after a drop the honest move is to reconnect and re-read.

**Replay.** The hub holds each session's last 400 events so a reconnecting page can be caught
up (`snapshot`, or the SSE replay). Fragments of a stream whose `done` has been published are
dropped from the buffer when it is written — a page that reloads gets the committed entry from
`GET /api/session/{id}`, and replaying the fragments too would render the same answer twice.

**Backpressure.** A client that cannot keep up is closed, not slowed: the publish never blocks,
and a full queue costs the connection, not the agent loop. A snapshot that cannot fit the queue
drops the frames it cannot deliver and logs how deep the hole got.

### What the shipped UI does with all this

If the websocket drops it does not give up on it: it retries with a 0.6 s to 15 s backoff
forever and, after a few failures, subscribes to SSE (or a 1.5 s poll) as a second channel,
saying which of the two it is living on and when the next socket attempt is due — a socket that
comes back closes the fallback again. The status line is the transport's own report, never a
guess. Two tabs on one session stay in sync from these events, and a tab re-reads the path when
the tip moves to an entry it has never seen: another tab moved the path or appended to it while
this one was in the background.

## Static files and mounts

- Everything that is not an `/api/`, `/ws`, `/testdata/` or `/debug/pprof/` path is the UI:
  files that exist are served (with `ETag` and `Cache-Control` — assets get 60 s and revalidate
  to a 304; `index.html` always revalidates), unknown paths without a file extension fall back
  to `index.html` (single-page app), unknown names with an extension are 404. The named UI
  directory wins over the copy embedded in the binary (`-web`), which is what makes an edit
  under `web/` show up without a rebuild.
- **`/testdata/…`** serves a development directory when one was named (`ServeTestdata`); off by
  default, never cached, no listing, no SPA fallback — a missing file is a 404.
- **`/debug/pprof/…`** is Go's runtime profiler on the API port, only when the operator asked
  (`-pprof same`).
- The websocket accepts any origin: authentication is expected to terminate at a reverse proxy,
  and the default deployment is a local port.
