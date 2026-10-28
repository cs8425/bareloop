# bareloop

A server-side agent loop with context compaction and a web UI. Everything a model can do comes
from MCP servers — the program ships **no built-in tools**.

It is meant to sit next to a local model: the core is deliberately nothing but the loop and the
tool loop's bookkeeping, and it talks to llama.cpp, vLLM, Ollama or LM Studio through their
OpenAI-compatible endpoint.

- Go, standard library only, plus `github.com/coder/websocket` for the browser transport.
- No database: one append-only JSONL log per session, where every entry is a node of a
  conversation tree and a `tip` record says which node is current.
- The agent loop runs on the server (not in the browser), so a run survives a page reload.
- Context compaction follows [`notes/compact-threshold-mode.en.md`](notes/compact-threshold-mode.en.md).
- MCP **2026-07-28** and **2025-11-25**, over stdio and streamable HTTP.
- Multi-session: every session has its own log, MCP server set, system prompt, sampling
  parameters, context window and compaction parameters.

## Build

```sh
go mod tidy
go build ./...
```

Go 1.22 or newer (the HTTP routes use the method-and-wildcard patterns added in 1.22).

## Run it against mocks

```sh
go build -o bin/ ./cmd/...
./bin/llmmock  -addr 127.0.0.1:8901 -nctx 4096 &
./bin/mcpstub  -transport stdio -version 2026-07-28      # used from config.json
./bin/mcpstub  -transport http  -addr 127.0.0.1:8911 &   # optional
cp config.example.json config.json
./bin/bareloopd -config config.json -addr 127.0.0.1:8090
```

Open <http://127.0.0.1:8090>. The mock answers with markdown, calls a tool when the prompt
contains `tool`, answers slowly when it contains `slow` (to test the stop button) and produces a
long reply when it contains `big` (to test compaction). A real provider works the same way:

```sh
BARELOOP_API_KEY=sk-... BARELOOP_MODEL=gpt-4.1-mini ./bin/bareloopd -config config.json
```

`config.json` is optional and `${VAR}` inside it is expanded from the environment, so secrets can
stay out of the file.

## Layout

| path | what lives there |
|---|---|
| `internal/ir` | the entry tree: entries, kinds, roles, usage, the path from the tip, effective (post-compaction) entries |
| `internal/store` | the append-only JSONL log: entries, the tip, patches, deletes, session metadata, replay |
| `internal/asm` | the **only** place that turns entries into a provider payload, plus per-message sizes |
| `internal/llm` | one OpenAI-compatible provider with usage parsing for OpenAI, llama.cpp and Ollama |
| `internal/mcp` | JSON-RPC 2.0, stdio and streamable-HTTP transports, both protocol eras |
| `internal/compact` | compaction: parameters, triggers, the cut, the summary request, the gate |
| `internal/agent` | sessions, the manager, the loop, tool execution, compaction commits |
| `internal/server` | the JSON API, the websocket stream, the SSE fallback, the static UI |
| `web` | the UI: HTML, CSS, ES modules, no build step |
| `cmd/bareloopd` | the server |
| `cmd/llmmock` | a fake OpenAI-compatible server, for testing |
| `cmd/mcpstub` | a fake MCP server (both eras, both transports), for testing |

## Configuration

`config.example.json` documents every field. `defaults` is inherited by every session; each
session stores its own copy in `data/<id>/config.json`.

| field | meaning |
|---|---|
| `addr`, `data`, `web`, `fsync`, `debug` | listen address, log directory, static UI directory, synchronous appends, request logging |
| `defaults.llm` | `kind` (`openai`), `baseUrl`, `apiKey`, `model`, `nCtx`, `cacheIncludedInPrompt`, `headers` |
| `defaults.systemPrompt` | the system prompt: the one place that decides language and script (see "What is actually sent") |
| `defaults.mcp[]` | `name`, `type` (`stdio`/`http`), `command`/`args`/`env`/`dir`, `url`/`headers` |
| `defaults.compact` | compaction parameters (see below): `auto` is off by default, `inLoop` is **on** by default |
| `defaults.asm` | payload options: `sendReasoning` (**on** by default), `sendImages` (**on**), `omitToolResultName` (off) |
| `defaults.temperature`, `maxTokens`, `maxTurns`, `reasoningEffort`, `extra` | sampling defaults (see below) |
| `sessions[]` | optional sessions to create at startup |

Keys are lowerCamelCase, and they are matched case-insensitively (`"SendReasoning"` is understood).
Two things bite harder than a typo:

- `defaults` is read when a session is **created**. Editing `config.json` afterwards changes nothing
  for an existing session — `data/<id>/config.json` is the truth, and a settings save writes the
  whole effective config back into it, which is how `"sendReasoning": true` appears there even
  though nobody typed it.
- `data/<id>/config.json` is rewritten by the server, so keep hand edits for the moments the
  server is not running: stop it, edit, start. The UI dialog and `PATCH` are the live paths.

`PATCH` follows one rule for every field: **the zero value means "keep what is stored"** — that is
how an empty API key keeps the key, and why `compact: {}` means "unchanged". It also means you
cannot clear a system prompt or switch `asm.sendReasoning` back to its unset state over the API;
stop the server, edit `data/<id>/config.json` and start again for that.

### Session identity

The session **id is its directory name**: `data/test-1/` *is* session `test-1`, and the `"id"` field
inside `data/test-1/config.json` is a copy of that name, nothing more. Whenever a session is loaded
the two are compared, and a file that disagrees is reported and rewritten:

```text
session "test-1": config.json says id "test"; the directory name wins, rewriting config.json
```

Nothing else is allowed to re-home a session — the log path, the session list, `PATCH` and `DELETE`
all work from the directory. The reason is concrete: a directory whose config carried another
session's id became that session, so messages typed into one were appended to the other's log while
its own `log.jsonl` was never read.

An id is a directory name, so it is validated on the way in: lowercase `a-z 0-9 . _ -`, at most 64 characters,
no `..`. Lowercase is not cosmetics: directory names are case-insensitive on Windows and macOS, so with
case folded away two sessions cannot differ only in how they are spelled, which is what lets the
filesystem itself guarantee uniqueness. `POST /api/sessions` requires an id and answers **400** without
one (it used to invent `s<timestamp>`, which left a session nobody asked for in the data directory);
creating over an existing directory is a **409**, and `DELETE` refuses an id that is not a valid name.
The API still refuses to guess, but the UI will: it suggests an id (`quiet-otter-1f3`) when the dialog
opens, offers another on request, folds what you type into lowercase, and makes one up if you clear the
field — because the person pressing create asked for it, in front of them. The id is not the label you
see either: `name` is free-form, so spaces and non-ASCII text belong there.

### Leaving a value out

Nothing is put into the request body unless it is set, so an unset field means the LLM server
keeps its own default:

| field | means "do not send / use the default" | sent when |
|---|---|---|
| `temperature` | `null`, or the key is absent | any number — **`0` is a real value** (greedy decoding) |
| `maxTokens` | `0` or absent | `> 0` (`max_tokens`) |
| `reasoningEffort` | `""` or absent | non-empty (`minimal`/`low`/`medium`/`high`) |
| `maxTurns` | `0` or absent = **no cap**: a run ends when the model stops asking for tools | `> 0` = hard cap per agentic run |
| `compact.auto` | absent = off (T1 after a turn) | `true` |
| `compact.inLoop` | absent = **on** (T2 between turns) | `false` turns it off |
| `asm.sendReasoning` | absent = **on** (assistant `reasoning_content` is re-sent) | `false` turns it off |
| `asm.sendImages` | absent = **on** (images go as `image_url` data URLs) | `false` sends one line about the image instead of its bytes |

`defaults` is merged when a session is **created**; afterwards `data/<id>/config.json` is the
truth for that session and is re-read on restart.

The context window (`llm.nCtx`) is probed from `/props`, `/model` and `/models` when it is not
configured (llama.cpp, vLLM, Ollama and LM Studio all publish it under one of
`n_ctx`, `max_context`, `context_length`, `max_model_len`). When it stays unknown the trigger
cannot fire and only a manual compaction does something.

### Images

An attached image is stored in the log as base64 and goes on the wire as an `image_url` part with
the message's own text next to it: an attachment does not replace what you typed, and a tool result
keeps its text beside its screenshot.

Whether a server can see at all is one request away: llama.cpp's `/props` answers with
`{"modalities": {"vision": true, …}}`, and `false` there means no projector is loaded. A text-only
server — a plain GGUF with nothing attached — has to parse the part anyway, and the base64 is charged
to the context whether anyone looks at it or not. For those,
`"asm": {"sendImages": false}`: the payload then carries one line instead of the bytes,

    [attached image4.png (image/png, ~184 KB) is in this conversation but its pixels were NOT
    sent to you: this session sends text only. You cannot see it, so do not describe it and do
    not guess what it shows — say that you cannot see the image and ask the user …]

which is worded as an instruction because the polite version does not work: a note that only mentions
that a picture was in the conversation gets answered with an invention. In a real session a model
produced a confident four-point description of an image it had never received, and its own reasoning
channel said why — "the prompt says the image was not sent … but since I am a multimodal model, I can
actually see the image". If `sendImages` is off, the only honest answer is "I cannot see it".

The UI always shows the real image, and an attachment stored as a reference rather than as bytes is
shown as that reference: a path is more use than a broken image icon.

### MCP notes

`2026-07-28` is stateless: no `initialize`, every request carries its identity in `params._meta`,
`server/discover` is the only discovery probe, `resultType` marks a final result, and multi
round-trip requests (a tool that needs input) replace server-initiated requests. The client probes
`server/discover` and falls back to the `2025-11-25` `initialize` handshake when a server does not
know it. `x-mcp-header` schema properties are mirrored into `Mcp-Param-*` HTTP headers, and an
answered input request is retried under a **new** JSON-RPC id.

A tool call whose server asks for input raises an elicitation request in the UI (the same dialog
serves legacy server-initiated `elicitation/create` requests).

## Web UI

`web/` is plain HTML/CSS/ES modules — no build step, no framework, served straight from disk
(`-web`, default `./web`). Events arrive over a websocket, falling back to SSE and then to a
1.5 s poll; every action is a REST call, so a button never waits on the event transport.

A few behaviours are worth knowing, because each of them was a bug first:

- **Following is opt-out.** The transcript scrolls with the output only while you are at the
  bottom; scrolling up (or wheeling up) unpins immediately and a floating `↓ latest` counts what
  you missed — Escape or the button jumps back. A wall of tokens must never steal the viewport
  from the composer, because that is where stop and interrupt live. `#log` is the only scrolling
  container (`min-height: 0` on the flex items is what keeps the document itself from scrolling).
- **Order comes from entry ids.** Entries are stored in a map and served by `GET
  /api/session/{id}`; the handler sorts them by their numeric id and the UI sorts again, so a
  reload renders the same sequence that was produced. `status.tip` marks the current path —
  entries that are not on it are shown dimmed, because they are not sent.
- **Caching is bounded, and cheap to revalidate.** `index.html` — the file that names every asset —
  is `Cache-Control: no-cache, must-revalidate`, so navigating to the UI always asks the server.
  Everything else is `public, max-age=60, must-revalidate`. Both carry an `ETag`, so a
  revalidation costs a 304 and no body, and a hard refresh always bypasses the lot. Editing `web/`
  while the server runs therefore shows up on the next navigation (or immediately on reload).

Below 860px the UI is one column: the session list becomes a drawer (☰, closed by the scrim, by
picking a session or by Escape) and the side panel becomes a bottom sheet. Heights use `dvh`
because on a phone `100vh` is the *largest* viewport and would hide the composer — the composer
is where stop and interrupt live — behind the browser's own bar; inputs are never smaller than
16px because iOS Safari zooms the page otherwise; and padding adds `env(safe-area-inset-*)` so
the notch and the gesture bar do not cover anything.

### Embedding the UI

The UI is plain files, served from `-web` (default `./web`, looked up in a few usual places so
`go run ./cmd/bareloopd` works from the repo root). Shipping a single binary means replacing that
directory with an embedded filesystem — `//go:embed all:web` plus `http.FS`, one import in `cmd`
and one field in `server.Server`:

- keep the split: the entry point is revalidated, assets have a lifetime;
- the `ETag` is the only thing that has to change meaning — from "size + modification time" to a
  hash of the bytes, since embedded files all share the build time;
- if the asset names ever grow content hashes (`app.4f9c1.js`), drop `max-age` to a year and hash
  the name into `index.html` instead; until then 60s is the staleness budget that lets an upgrade
  self-heal.

## What is actually sent

`GET /api/session/{id}/inspect` answers the size question (per-message tokens, budget, plan, tool
list) and now also returns the `system` head verbatim; `GET /api/session/{id}/payload` answers
the content question with the exact body of the next request — `{system, messages, tools}`, the
same `[]Message` values the provider receives. The inspect panel shows both (`payload as sent`),
so no part of the prompt is hidden behind a byte count.

Every request also leaves one line in the process log:

```
turn 3: 14 messages + system prompt (287 bytes), 9 tools, 41233 bytes
```

and `BARELOOP_PAYLOAD_DUMP=1` appends the whole body as JSON after it — large, and only worth
turning on while debugging. Use it whenever the model behaves oddly: it settles in one line
whether something other than the user's own words is steering the answer.

Language and script are not free. The assembler sends no text of its own (there is not a single
non-ASCII string in the Go code or the UI), so the model answers in whatever it was trained to
prefer unless the system prompt says otherwise — which is how a Traditional Chinese question
comes back in Simplified. `defaults.systemPrompt` in `config.example.json` pins both: same
language, same script, no unrequested switching.

### When a request fails

A provider error is retried at most twice, and only where a retry can plausibly help: a transport
failure (the request never got an answer), 429, and the 5xx family. A 400/401/403/413/422 is an
answer about the request itself and will be identical forever, so it is not retried at all.

Either way, **the turn ends with an entry**. If nothing ever came back, the loop commits an assistant
message that says so —

    [generation failed: llm: 500 Internal Server Error: {"error": …}]

— and the UI draws it as a failure rather than as an answer. That commit is not decoration: the loop
owes an answer exactly while the path ends with a user or a tool message, so committing nothing
leaves it owing one forever. A provider that answers 500 reliably then produces a storm — one
identical request per round, each with a payload line and an error event, forever. As the belt to
those braces, a round that committed nothing stops the run instead of starting another round.

## HTTP API

| method | path | what it does |
|---|---|---|
| GET | `/api/health` | liveness |
| GET | `/api/sessions` | every session with its status |
| POST | `/api/sessions` | create a session (body: a config; `id` is required, lowercase, and becomes the directory name — 400 without it, 409 if the directory exists) |
| GET | `/api/session/{id}` | config (without the API key), status, every entry sorted by id, pending ids |
| PATCH | `/api/session/{id}` | update the config; refuses while a run is active; a field the body does not carry is kept, not cleared |
| DELETE | `/api/session/{id}` | close and delete the session |
| POST | `/api/session/{id}/send` | `{text, images[], files[], label}` — send now or queue |
| POST | `/api/session/{id}/interrupt` | abort the current generation and send this message instead |
| POST | `/api/session/{id}/stop` | abort the current run, keep what was produced |
| POST | `/api/session/{id}/compact` | `{instruction}` — compact now, even below the threshold |
| POST | `/api/session/{id}/gate` | `{id, answer}` — `accept` / `retry` / `skip` for an oversized summary |
| POST | `/api/session/{id}/elicit` | `{id, behavior, content}` — answer a tool's input request |
| POST | `/api/session/{id}/entry/patch` | `{id, content}` — edit a message (branches automatically) |
| POST | `/api/session/{id}/entry/delete` | `{id}` — delete a leaf or make an interior entry synthetic |
| POST | `/api/session/{id}/tip` | `{id}` — move the tip (time travel) |
| POST | `/api/session/{id}/reload` | re-read the log from disk |
| GET | `/api/session/{id}/inspect` | what would be sent: the system head, per-message token sizes, the plan, the tool list |
| GET | `/api/session/{id}/payload` | the exact body of the next request: `{system, messages, tools}` |
| GET | `/api/session/{id}/events` | the event stream as SSE (fallback transport) |
| GET | `/ws` | the event stream as a websocket (primary transport) |

### Websocket protocol

Text frames, JSON. The server sends one object per frame: `{type, session, seq, ts, ...}`, plus
`{type:"reply", req, data}` / `{type:"reply_error", req, error}` answers. The read limit is
switched off, because images travel as base64 inside one message.

Commands a browser may send: `ping`, `subscribe` (`{session: "a,b"}`, empty means all),
`snapshot`, `send`, `interrupt`, `stop`, `compact`, `gate`, `elicit`, `inspect`, `status`,
`pending`, `entryPatch`, `entryDelete`, `tip`, `name`. Every action also exists as REST, which is
what the bundled UI uses.

Event types: `hello`, `entry`, `delta`, `tool_call`, `tool_result`, `state`, `pending`,
`compaction`, `request`, `request_done`, `log`, `info`, `error`, `removed`.

### Message queue

A message that arrives while the loop is running is appended to the path like any other entry,
which makes it *pending*. It is injected at the next turn boundary, after the tool result that
belongs to the current turn, or as soon as compaction finishes. `/interrupt` stops generation
first, keeps what was produced, and sends the message immediately.

## Compaction in one paragraph

The path is the truth and exactly one assembler decides what leaves the process. A trigger is
`contextUsed > n_ctx × threshold%`, where `contextUsed` is the reported usage of the last
completed turn plus a token count of everything appended since. The cut keeps the newest suffix
that fits `keepTarget%` of the window, never inside a tool span and never before a tool result,
and never fewer than `minKeptUserTurns` user turns. Everything above it is summarised by a
request whose prefix is byte-identical to the last payload actually sent, so the provider's prompt
cache survives. The commit writes the boundary entry and moves the tip in one log record; the old
branch stays intact, so any compaction can be undone by moving the tip back. Acceptance is
size-based (with one retry and then a three-option gate), failures fall open, and the summary
enters the payload as one `user` message with a fixed marker prefix. `Inspect` shows the exact
numbers the decisions were made from.

## Markdown rendering

`web/js/md.js` is a dependency-free renderer that escapes first and only then builds markup, so
pasted model output cannot inject script. It exports `{ register, render, renderTo, escape }` and
also sets `window.bareloopMarkdown`, which is the extension point:

```html
<script type="module">
  import md from './js/md.js';
  md.register({
    name: 'mermaid',
    // code hooks see the fence body and language; return null to let the default render it
    code: (code, lang) => (lang === 'mermaid' ? `<pre class="mermaid">${code}</pre>` : null),
  });
  md.register({
    name: 'math',
    // inline hooks run after the built-in inline pass
    inline: (text) => text.replace(/\$\$(.+?)\$\$/g, '<i>[$1]</i>'),
  });
  md.register({
    name: 'callout',
    // block hooks see the line array, the index, the output array and helpers;
    // return the index of the next line to process, or -1 when the line was not yours
    block(lines, i, out, helpers) {
      if (!lines[i].startsWith('!!!')) return -1;
      out.push(`<p class="callout">${helpers.escape(lines[i].slice(3))}</p>`);
      return i + 1;
    },
  });
</script>
<script type="module" src="js/app.js"></script>
```

Hooks run in registration order; the first `code` hook that returns a string wins. A `script`
tag of `type="module"` is deferred, so an extension module placed before `app.js` still sees the
renderer; a classic inline script would run too early.

## Tests

`go test ./...` runs the unit tests of the packages that have them. The binaries in `cmd/llmmock`
and `cmd/mcpstub` exist so the whole loop — streaming, tool calls, usage accounting, compaction,
abort — can be exercised without external services.
