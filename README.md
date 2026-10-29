# bareloop

A server-side agent loop with context compaction and a web UI. Everything a model can do comes from MCP
servers - the program ships **no built-in tools**.

It is meant to sit next to a local model: the core is deliberately nothing but the loop and the tool
loop's bookkeeping, and it talks to llama.cpp, vLLM, Ollama or LM Studio through their OpenAI-compatible
endpoint.

## What it is like

- **Go, standard library only**, plus `github.com/coder/websocket` for the browser transport. No
  framework, no build step for the UI.
- **No database.** One append-only JSONL log per session, where every entry is a node of a conversation
  tree and a `tip` record says which node is current. Moving the tip is time travel: the file is only ever
  appended to, so an abandoned path stays on disk. It is a tree, but the UI shows one path at a time: what
  is not on the path is not drawn, and there is no branch switcher yet (`docs/design.md`, Deferred).
- **The loop runs on the server**, not in the browser, so a run survives a page reload and survives
  closing the tab.
- **Nothing is hidden from you.** `inspect` shows the token count of every message and the exact numbers
  the compaction decisions were made from, `payload` shows the literal body of the next request, and every
  request leaves one line in the process log.
- **Context compaction that survives a huge single message.** When the conversation does not fit the
  window the summary is produced serially, in parts that each fit; a message too large to fit on its own is
  read at its beginning and its end and the summariser is told the middle was omitted.
- **The window is discovered, not assumed**: the effective `n_ctx` is the smaller of your ceiling and what
  the server reports, and a session that outgrows the window is repaired instead of stuck.
- **Multi-session**: every session has its own log, MCP server set, system prompt, sampling parameters,
  context window and compaction parameters.
- MCP **2026-07-28** and **2025-11-25**, over stdio and streamable HTTP, with a dialog in the UI whenever a
  tool needs input.
- Context compaction follows the contract in
  [`notes/compact-threshold-mode.en.md`](notes/compact-threshold-mode.en.md).

| read this | for |
|---|---|
| this file | what it is, how to build it, how to use it |
| [`docs/design.md`](docs/design.md) | how it works and why: the context window, compaction invariants, prompt cache, retries, MCP eras, the UI's shape, the markdown renderer's hooks, and what is deliberately deferred |
| [`notes/compact-threshold-mode.en.md`](notes/compact-threshold-mode.en.md) | the compaction spec (the authority when the two docs disagree) |
| [`config.example.json`](config.example.json) | every config field, documented inline |

## Build

```sh
go mod tidy
go build ./...
```

Go 1.27 or newer is what `go.mod` asks for ([why](docs/design.md#go-version)).

## Run it against mocks

```sh
go build -o bin/ ./cmd/...
./bin/llmmock  -addr 127.0.0.1:8901 -nctx 4096 &
./bin/mcpstub  -transport stdio -version 2026-07-28      # used from config.json
./bin/mcpstub  -transport http  -addr 127.0.0.1:8911 &   # optional
cp config.example.json config.json
./bin/bareloopd -config config.json -addr 127.0.0.1:8090
```

Open <http://127.0.0.1:8090>. The mock answers with markdown, calls a tool when the prompt contains
`tool`, answers slowly when it contains `slow` (to test the stop button) and produces a long reply when it
contains `big` (to test compaction). A real provider works the same way:

```sh
BARELOOP_API_KEY=sk-... BARELOOP_MODEL=gpt-4.1-mini ./bin/bareloopd -config config.json
```

`config.json` is optional and `${VAR}` inside it is expanded from the environment, so secrets can stay out
of the file. `-data` is where sessions live (default `./data`), `-web` where the UI files are (default
`./web`).

The UI puts the session in the URL (`#/s/<id>`): reload, bookmark or paste the link and you are back at that
conversation, the back button steps through the sessions you looked at, and a link naming a session that is
not there says so and puts the current one back into the address bar.

## Using it

The transcript is the log: what you see is what is stored. In the composer **Enter** sends, **Shift+Enter**
adds a line, and you can **paste or drop images** (and files) next to your text. While a run is going,
**stop** keeps what was produced and **interrupt** aborts the current generation and sends what you just
typed instead; a message sent mid-run is queued and injected at the next turn boundary. Scrolling up
unpins the view from the streaming output, and `↓ latest` takes you back.

Per session you can:

- **copy a message**, **see its raw text**, or **delete an entry** (its children are re-attached to its
  parent);
- **run again from any legal break point** - a user message or a tool result. That
  moves the path to that entry and continues the loop **without appending anything**: the tool result that
  already arrived is reused, not redone. This is what to press when a run died half way (output stopped
  mid-reasoning, a tool result with no answer after it, "the context was compacted, please send again").
  Everything that followed leaves the path - the transcript draws the path only, and the confirm box counts
  what you are giving up; it stays in the log either way;
- **re-make a compaction summary** - the same button on a summary entry means "make this summary again from
  the messages before it": the path moves back to where the summary was made (so the messages it sees are
  the ones it saw then) and a fresh summary is asked for with the prompt this trigger writes today. The old
  summary and everything after it leave the path. The run continues afterwards **only** if that summary had
  been written in the middle of a run (`in_loop`, which is also what the forced compaction after a provider
  refused the payload writes); an `end_of_turn` or a manual one replaces the summary and leaves the session
  stopped - what had stopped stays stopped;
- **compact now**, optionally with an instruction ("keep every file path mentioned"), even below the
  threshold; if the summary comes back too large you get the three-option gate (accept / retry / skip);
- **answer a tool's input request** in a dialog, for MCP servers that ask mid-call;
- **change settings** - model, `n_ctx`, system prompt, sampling, MCP servers, compaction thresholds -
  while the session is idle;
- **open the tool catalogue** by clicking the `N tools` badge: every tool this session publishes, per server,
  with what it is for, the parameters the model is being asked to fill in, what the definition costs every
  request, and how often this transcript used it. The switches live there too - `disabledTools` per tool, a
  `toolPrefix` per server, the whole server on or off - next to what a change costs (the tool block is sent in
  front of the history, so replacing it costs one re-prefill, which the drawer prices) and, while a run is
  going, the fact that the change waits for the run to end, with a button to end the run and apply it;
- **inspect** what would be sent: per-message tokens, the compaction plan, the tool list, and the exact
  request body.

A session's `id` is its directory name under `data/`: lowercase `a-z 0-9 . _ -`, at most 64 characters
([the rules and their reasons](docs/design.md#session-identity)). The `name` you see in the sidebar is
separate and free-form, so spaces and non-ASCII text belong there.

## Configuration

`config.example.json` documents every field. `defaults` is inherited by every session; each session stores
its own copy in `data/<id>/config.json`.

| field | meaning |
|---|---|
| `addr`, `data`, `web`, `fsync`, `debug` | listen address, log directory, static UI directory, synchronous appends, request logging |
| `defaults.llm` | `kind` (`openai`), `baseUrl`, `apiKey`, `model`, `nCtx`, `headers` |
| `defaults.systemPrompt` | the system prompt: the one place that decides language and script |
| `defaults.mcp[]` | `name`, `type` (`stdio`/`http`), `command`/`args`/`env`/`dir`, `url`/`headers` |
| `defaults.compact` | compaction parameters (see below): `auto` is off by default, `inLoop` is **on** by default |
| `defaults.asm` | payload options: `sendReasoning` (**on** by default), `sendImages` (**on**), `omitToolResultName` (off) |
| `defaults.temperature`, `maxTokens`, `maxTurns`, `reasoningEffort`, `extra` | sampling defaults (see below) |
| `sessions[]` | optional sessions to create at startup |

Keys are lowerCamelCase, and they are matched case-insensitively (`"SendReasoning"` is understood).
Two things bite harder than a typo:

- `defaults` is read when a session is **created**. Editing `config.json` afterwards changes nothing for an
  existing session - `data/<id>/config.json` is the truth, and a settings save writes the whole effective
  config back into it, which is how `"sendReasoning": true` appears there even though nobody typed it.
- `data/<id>/config.json` is rewritten by the server, so keep hand edits for the moments the server is not
  running: stop it, edit, start. The UI dialog and `PATCH` are the live paths.

`PATCH` follows one rule for every field: **the zero value means "keep what is stored"** - that is how an
empty API key keeps the key, and why `compact: {}` means "unchanged". It also means you cannot clear a
system prompt or switch `asm.sendReasoning` back to its unset state over the API; stop the server, edit
`data/<id>/config.json` and start again for that.

### Leaving a value out

Nothing is put into the request body unless it is set, so an unset field means the LLM server keeps its own
default:

| field | means "do not send / use the default" | sent when |
|---|---|---|
| `temperature` | `null`, or the key is absent | any number - **`0` is a real value** (greedy decoding) |
| `maxTokens` | `0` or absent | `> 0` (`max_tokens`) |
| `reasoningEffort` | `""` or absent | non-empty (`minimal`/`low`/`medium`/`high`) |
| `maxTurns` | `0` or absent = **no cap**: a run ends when the model stops asking for tools | `> 0` = hard cap per agentic run |
| `compact.auto` | absent = off (compaction after a turn) | `true` |
| `compact.inLoop` | absent = **on** (compaction between turns) | `false` turns it off |
| `compact.summaryMode` | absent = `prefix-stable` (one summary request; falls back to the serial chain when the region cannot fit one request) | `"iterative"` always uses the chain; `"map-reduce"` is reserved and currently runs the chain |
| `asm.sendReasoning` | absent = **on** (assistant `reasoning_content` is re-sent) | `false` turns it off |
| `asm.sendImages` | absent = **on** (images go as `image_url` data URLs) | `false` sends one line about the image instead of its bytes |

`llm.nCtx` is a ceiling you set, not a fact about the server: the number used is the smaller of it and
what the server reports, and `status.nCtx` / `nCtxSource` tell you which won. Leave it unset and the
server is asked; if neither knows, compaction stays off until you fill it in
([the five cases](docs/design.md#the-context-window)).

### Tools: switches, prefixes and the cache

A tool is published under its **own name**: `fs_list`, not `mcp-vroot__fs_list`. Nothing is namespaced
unless you ask for it, because a tool whose name needs a prefix to be understood is a badly named tool.
Three things are configured **per session**, all in `mcp[]`:

| field | default | what it does |
|---|---|---|
| `disabled` | `false` | do not connect to this server at all (this used to be a dead key; the `mcp-` trick in a config is no longer needed) |
| `disabledTools` | none | hide these tools from the model; matches the **raw** name (`fs_list`) exactly or as a `path.Match` glob (`fs_*`, `*`), case sensitive |
| `toolPrefix` | none | namespace in front of this server's tools: `"vr"` ⇒ `vr_fs_list` (one underscore) |

Two tools that want the same name are a collision, and the **later** one moves: it is published as
`<server>_<tool>`, never as `fs_read_2`, because a name you cannot read is a name you cannot use. It is
logged when it happens — `tool name collision: beta/fs_read would be "fs_read", which another tool
already owns; publishing it as "beta_fs_read" instead` — and `GET .../tools` reports the same fact per tool
as `renamedFrom`, so the drawer can say why a switch is called what it is. The first claimer never moves:
renaming tools every time a server is added would invalidate the KV prefix of every session that already
has history. A prefix changed and then changed back hands the original names straight back, and every name
a tool was published under stays resolvable to it, so old history never becomes unrunnable.

Hiding a tool is cheap in the direction you want to think about: the schema leaves the request, so the
prompt shrinks (~118 tokens per small tool on a Gemma-class model, ~165 on Qwen-class) **and** the prefix
cache breaks, because `tools` is serialized after the system block — so the next request computes the
history behind it again. No seconds are quoted here on purpose: a prefill rate belongs to one server at
one context size, and the two single measurements that once said "+14.3 s / +22.8 s" moved 10–19 % when the
same servers were measured again. The only cost-free moment is
before the first request of a session. That is why `GET /api/session/{id}/tools` reports `estTokens` per
tool, a `contextUsed` figure, and `pendingChanges`.

What the model may call, and what each tool wants, is the same call: per server the prefix and connection
state, per tool the published name, the raw name, `title`, `description`, `schema` (its input schema, i.e.
its parameters), `estTokens`, `uses`. The tools drawer renders exactly that, so switching a tool off never
means hunting for a name in a server's source first.

Changes apply at the **next run**: a run in flight keeps the tool array it started with, because switching
mid-run would break the prefix cache and strand a `tool_call` whose tool vanished. `pendingChanges` says
when something is waiting; `toolsPending` in the status says the same thing in one bit.

### Compaction, in short

A turn is folded away when `contextUsed > n_ctx × threshold%`: the newest suffix that fits `keepTarget%`
of the window is kept (never inside a tool span, never fewer than `minKeptUserTurns` user turns) and
everything above the cut becomes one summary message. The old branch stays in the log, so a compaction can
be undone by moving the tip back. Failures fall open - the worst case is a session that keeps more context,
never one that loses it ([details](docs/design.md#compaction-in-one-paragraph)).

### Debugging what the model actually saw

```sh
curl -s localhost:8090/api/session/main/payload   # the exact body of the next request
BARELOOP_PAYLOAD_DUMP=1 ./bin/bareloopd ...        # and dump every request to the log
```

Every turn also logs one line - `turn 3: 14 messages + system prompt (287 bytes), 9 tools, 41233 bytes`.
Language and script are not free: the assembler adds no prose of its own, so unless the system prompt says
otherwise the model answers in whatever it prefers
([why the system prompt is the only lever](docs/design.md#what-is-actually-sent)).

## HTTP API

| method | path | what it does |
|---|---|---|
| GET | `/api/health` | liveness |
| GET | `/api/sessions` | every session with its status |
| POST | `/api/sessions` | create a session (body: a config; `id` is required, lowercase, and becomes the directory name - 400 without it, 409 if the directory exists) |
| GET | `/api/session/{id}` | config (without the API key), status, every entry sorted by id, pending ids |
| PATCH | `/api/session/{id}` | update the config; refuses while a run is active; a field the body does not carry is kept, not cleared |
| DELETE | `/api/session/{id}` | close and delete the session |
| POST | `/api/session/{id}/send` | `{text, images[], files[], label}` - send now or queue |
| POST | `/api/session/{id}/interrupt` | abort the current generation and send this message instead |
| POST | `/api/session/{id}/stop` | abort the current run, keep what was produced |
| POST | `/api/session/{id}/resume` | continue from the current tip without appending anything; the path must end on a user message, a tool result or a compaction summary |
| POST | `/api/session/{id}/compact` | `{instruction}` - compact now, even below the threshold |
| POST | `/api/session/{id}/gate` | `{id, answer}` - `accept` / `retry` / `skip` for an oversized summary |
| POST | `/api/session/{id}/elicit` | `{id, behavior, content}` - answer a tool's input request |
| POST | `/api/session/{id}/entry/patch` | `{id, content}` - rewrite one entry's content **in place**: every path through it changes too, and it does not branch |
| POST | `/api/session/{id}/entry/delete` | `{id}` - delete a leaf or make an interior entry synthetic |
| POST | `/api/session/{id}/tip` | `{id}` - move the tip (time travel) |
| POST | `/api/session/{id}/reload` | re-read the log from disk |
| GET | `/api/session/{id}/inspect` | what would be sent: the system head, per-message token sizes, the plan, the tool list |
| GET | `/api/session/{id}/payload` | the exact body of the next request: `{system, messages, tools}` |
| GET | `/api/session/{id}/tools` | every server and every tool, with `enabled`, `estTokens`, `uses`, `prefix`, `schema` (its parameters), `renamedFrom` and `pendingChanges` |
| POST | `/api/session/{id}/tools` | `{server, disabledTools[], toolPrefix}` — set the per-session tool policy; `{}` reads it |
| GET | `/api/session/{id}/events` | the event stream as SSE (fallback transport) |
| GET | `/ws` | the event stream as a websocket (primary transport) |

### Websocket protocol

Text frames, JSON. The server sends one object per frame: `{type, session, seq, ts, ...}`, plus
`{type:"reply", req, data}` / `{type:"reply_error", req, error}` answers. The read limit is switched off,
because images travel as base64 inside one message.

Commands a browser may send: `ping`, `subscribe` (`{session: "a,b"}`, empty means all), `snapshot`, `send`,
`interrupt`, `stop`, `resume`, `compact`, `gate`, `elicit`, `inspect`, `status`, `pending`, `entryPatch`,
`entryDelete`, `tip`, `name`, `tools`, `toolsSet`. Every action also exists as REST, which is what the
bundled UI uses - so a button never waits on the event transport.

If the websocket drops the UI does not give up on it: it retries with a 0.6 s to 15 s backoff forever and,
after a few failures, subscribes to SSE (or a 1.5 s poll) as a second channel, saying which of the two it is
living on and when the next socket attempt is due - a socket that comes back closes the fallback again. The
status line is the transport's own report, never a guess. Two tabs on one session stay in sync from these
events, and a tab re-reads the path when the tip moves to an entry it has never seen: another tab moved the
path or appended to it while this one was in the background.

Event types: `hello`, `entry`, `delta`, `tool_call`, `tool_result`, `state`, `pending`, `compaction`,
`request`, `request_done`, `log`, `info`, `error`, `removed`, `tools` (the tool set was rebuilt; the body is
the same view `GET .../tools` returns).

### Message queue

A message that arrives while the loop is running is appended to the path like any other entry, which makes
it *pending*. It is injected at the next turn boundary, after the tool result that belongs to the current
turn, or as soon as compaction finishes. `/interrupt` stops generation first, keeps what was produced, and
sends the message immediately.

## Tests

`go test ./...` runs the unit tests of the packages that have them. The binaries in `cmd/llmmock` and
`cmd/mcpstub` exist so the whole loop - streaming, tool calls, usage accounting, compaction, abort - can be
exercised without external services.
