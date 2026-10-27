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
| `defaults.systemPrompt` | the system prompt |
| `defaults.mcp[]` | `name`, `type` (`stdio`/`http`), `command`/`args`/`env`/`dir`, `url`/`headers` |
| `defaults.compact` | compaction parameters (see below): `auto` is off by default, `inLoop` is **on** by default |
| `defaults.asm` | payload options: `sendReasoning` (**on** by default), `omitToolResultName` (off) |
| `defaults.temperature`, `maxTokens`, `maxTurns`, `reasoningEffort`, `extra` | sampling defaults (see below) |
| `sessions[]` | optional sessions to create at startup |

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

`defaults` is merged when a session is **created**; afterwards `data/<id>/config.json` is the
truth for that session and is re-read on restart.

The context window (`llm.nCtx`) is probed from `/props`, `/model` and `/models` when it is not
configured (llama.cpp, vLLM, Ollama and LM Studio all publish it under one of
`n_ctx`, `max_context`, `context_length`, `max_model_len`). When it stays unknown the trigger
cannot fire and only a manual compaction does something.

### MCP notes

`2026-07-28` is stateless: no `initialize`, every request carries its identity in `params._meta`,
`server/discover` is the only discovery probe, `resultType` marks a final result, and multi
round-trip requests (a tool that needs input) replace server-initiated requests. The client probes
`server/discover` and falls back to the `2025-11-25` `initialize` handshake when a server does not
know it. `x-mcp-header` schema properties are mirrored into `Mcp-Param-*` HTTP headers, and an
answered input request is retried under a **new** JSON-RPC id.

A tool call whose server asks for input raises an elicitation request in the UI (the same dialog
serves legacy server-initiated `elicitation/create` requests).

## HTTP API

| method | path | what it does |
|---|---|---|
| GET | `/api/health` | liveness |
| GET | `/api/sessions` | every session with its status |
| POST | `/api/sessions` | create a session (body: a config; `{}` is enough) |
| GET | `/api/session/{id}` | config (without the API key), status, entries on the current path, pending ids |
| PATCH | `/api/session/{id}` | update the config; refuses while a run is active |
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
| GET | `/api/session/{id}/inspect` | what would be sent: per-message token sizes, the plan, the tool list |
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
