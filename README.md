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
of the file. `-data` is where sessions live (default `./data`). The web UI is compiled into the binary, so
there is nothing else to copy to another machine; `-web <dir>` serves it from a directory on disk instead,
which is what you want while editing it, because an edit then shows up on the next reload with no rebuild.

The UI puts the session in the URL (`#/s/<id>`): reload, bookmark or paste the link and you are back at that
conversation, the back button steps through the sessions you looked at, and a link naming a session that is
not there says so and puts the current one back into the address bar.

Every URL the UI asks for is resolved against where its own code was loaded from, so it works behind a reverse
proxy that hangs it under a prefix — `/app/bareloopd/`, `/app/`, or `/` — as long as the proxy takes that prefix
off before forwarding, which is how one is normally written (`location /app/bareloopd/ { proxy_pass
http://127.0.0.1:8090/; }`, or `handle_path /app/*` in Caddy). The daemon always answers at `/`: there is no
base-path setting to keep in sync, and none to get wrong. Two things to know. The browser's URL keeps its
trailing slash (`/app/bareloopd/`, not `/app/bareloopd` — without the slash the page looks for its own files one
directory up). And the two long-lived channels want to be let through: the websocket `/ws` wants `Upgrade` and
`Connection` forwarded, and the event stream `/api/session/{id}/events` wants buffering off (`proxy_buffering off`;
Caddy and Apache stream by default). A proxy that does neither still works — the UI notices, says
`poll fallback` in the sidebar, and keeps going a poll interval behind.

## Using it

The transcript is the log: what you see is what is stored. In the composer **Enter** sends, **Shift+Enter**
adds a line, and you can **paste or drop images** (and files) next to your text. While a run is going **Enter
still just sends**: the message joins the path as *pending*, unless an answer is owed - for a tool call whose
result has not arrived, or for a request the model has been sent and has not answered - in which case the daemon
holds it and appends it right after that answer
([two moments for a message](#two-moments-for-a-message)). **stop** keeps what was produced, and
**interrupt** - the bolt, and the only gesture that reaches it - aborts the generation in progress and starts
the loop again on what you just typed. Scrolling up unpins the view from the streaming output, and
`↓ latest` takes you back.

A session asleep on a `wait` says so beside the phase - `asleep → 14:32`, the moment it is due, in the
browser's own clock - and still reports *running*, because everything there is to do with it - end it, or
interrupt it with a message - is something you do to a running session. `wakeAt` in the status is what carries
that moment, and `queued` is what counts the messages sent while waiting for it.

Per session you can:

- **copy a message**, **see its raw text**, or **delete an entry** (its children are re-attached to its
  parent);
- **run again from any legal break point** - a user message or a tool result. That
  moves the path to that entry and continues the loop **without appending anything**: the tool result that
  already arrived is reused, not redone. This is what to press when a run died half way (output stopped
  mid-reasoning, a tool result with no answer after it, "the context was compacted, please send again"), and
  also after **stop**: pressing it on the message nobody answered asks that message again, without writing a
  second copy of it. A run that ended at `maxTurns` stops on an unanswered tool call, which is a legal place to
  stop but not one to ask from - there, the button on the run's last tool result is the one that works.
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

Every session directory holds `config.json`, `log.jsonl` - the log itself, one record per line, append-only,
and the only file that has to survive - and `log.idx`, a derived index of where each record lies. The index
is rebuilt whenever the log is read, and a log that cannot write one keeps taking writes without it
([what is on disk, and why](docs/design.md#what-is-on-disk)). It is also what answers the session list, so a
session nothing has opened still appears in it with its numbers.

## Configuration

`config.example.json` documents every field. `defaults` is inherited by every session; each session stores
its own copy in `data/<id>/config.json`.

| field | meaning |
|---|---|
| `addr`, `data`, `web`, `fsync`, `debug` | listen address, log directory, UI directory (empty — the default — is the copy compiled into the binary), synchronous appends, request logging |
| `defaults.llm` | `kind` (`openai`), `baseUrl`, `apiKey`, `model`, `nCtx`, `headers`, `pricing` (`input` for the prompt tokens the server had to compute, `cachedInput` for the ones it served from cache, `output` for what it generated — **all three per 1 000 000 tokens**, and no currency field, because bareloop cannot know which one you are billed in. Leave it out and nothing is priced; an explicit `0` is an answer meaning free. A session inherits it and may override; a price changes no request, so saving one never reconnects anything) |
| `defaults.systemPrompt` | the system prompt: the one place that decides language and script |
| `defaults.mcp[]` | `name`, `type` (`stdio`/`http`), `command`/`args`/`env`/`dir`, `url`/`headers` |
| `insecureTLS`, `caFile` | on `defaults.llm` and on any `mcp[]` entry with a `url`: how an https endpoint is trusted ([reaching an endpoint no trust store knows](#reaching-an-endpoint-no-trust-store-knows)) |
| `proxy` | at three levels — `defaults.proxy` (every session's), `llm.proxy`, `mcp[].proxy`: the nearer answer wins, and empty leaves the environment in charge ([reaching an endpoint through a proxy](#reaching-an-endpoint-through-a-proxy)) |
| `defaults.builtinTools` | the tools the daemon answers itself, named one at a time: `wait`, `get_datetime`. It is an **on-list**, so all of them are off until a session says otherwise ([the daemon's own two tools](#the-daemons-own-two-tools)) |
| `defaults.timezone` | the zone the time tools answer in when a call names none: an IANA name (`Asia/Taipei`), a fixed offset (`+08:00`, `UTC+8`), `Local` for the machine's own - and nothing at all means UTC ([the daemon's own two tools](#the-daemons-own-two-tools)) |
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
empty API key keeps the key. It also means you cannot clear a system prompt or switch `asm.sendReasoning`
back to its unset state over the API; stop the server, edit `data/<id>/config.json` and start again for
that.

One block does not merge field by field: **`compact` is replaced whole.** The block is only treated as
"unchanged" when it carries nothing at all (`compact: {}`); the moment it carries anything, that block *is*
the new answer, and every key you left out comes back as its own default - `{"compact":{"auto":true}}` on a
session with `threshold: 50` resets the threshold to 70, turns `inLoop` back on, and drops a stored
`instruction`. **Read the config, change what you want, send the whole `compact` block back** - which is
what the settings dialog does, and why its own comment says a field it did not draw is not guessed at.

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
| `compact.threshold` `keepTarget` `summaryMax` `minSummarizeTokens` | `0`, negative or absent = the default (70 / 50 / 70 / 2000) — 0 here would be a budget of nothing, which is not an answer anyone means | `> 0`: percentages of `n_ctx`, and a token count for `minSummarizeTokens` |
| `compact.minKeptUserTurns` `minKeptMessages` | **`0` is a real answer = no floor**, and an omitted key decodes to that 0 — so the 4 that a *new* session starts with is not reachable by leaving the key out | `> 0` is the floor; a negative clamps to 0, never to the shipped default |
| `compact.maxAttempts` | `0` or absent = 3 tries for one summary | `> 0` tries; **a negative keeps trying until the summary fits** |
| `asm.sendReasoning` | absent = **on** (assistant `reasoning_content` is re-sent) | `false` turns it off |
| `asm.sendImages` | absent = **on** (images go as `image_url` data URLs) | `false` sends one line about the image instead of its bytes |
| `builtinTools` | absent = **every one of them off** — the list is what switches a builtin on | any list of real names (`["wait"]`); `[]` is a decision meaning "all off", and a name that is not a builtin is a 400 rather than a quiet no |
| `timezone` | absent = UTC | any zone the daemon can resolve — `Asia/Taipei`, `+08:00`, `UTC-5`, `Local`. Getting back to UTC means sending `"UTC"`, because empty means unchanged |
| `structuredContent` | absent = **`auto`** (fold the tool's JSON in, but never twice) | `fallback` / `always` / `drop`, per server in `mcp[]` ([the four words](#structured-content-in-four-words)) |
| `structuredMaxBytes` | absent = **16 384 bytes** of a tool's JSON are shown, then a note naming what was left out | any positive byte count; only a negative is refused, because it is what someone types for "no limit" and it decodes back as the default. Unlike the word above it, `defaults` does not carry it: the ceiling is one session's own. `0` means unchanged, so the shipped default only comes back by being typed |
| `llm.insecureTLS` | absent = verify, like anyone else | `false` is the same as absent; **`true` is a different endpoint** and is logged every time it applies |
| `llm.caFile` | absent = the system trust store | a PEM bundle to verify against — and the only thing it is verified against |
| `proxy` | absent = what the environment says (`HTTP_PROXY`, `NO_PROXY`) | `socks5://`, `socks5h://`, `http://` or `https://` — this session's answer for every endpoint that names none; `direct` means no proxy at all. An endpoint's own `proxy` wins over it ([through a proxy](#reaching-an-endpoint-through-a-proxy)) |
| `llm.proxy` | absent = the session's `proxy` | the same address, for this one endpoint |

`llm.nCtx` is a ceiling you set, not a fact about the server: the number used is the smaller of it and
what the server reports, and `status.nCtx` / `nCtxSource` tell you which won. Leave it unset and the
server is asked; if neither knows, compaction stays off until you fill it in
([the five cases](docs/design.md#the-context-window)).

### Reaching an endpoint no trust store knows

The llama.cpp box on the next desk with a certificate it made itself, or an MCP server under your own CA,
is refused by the default verifier. Two fields say what to do about that, on `llm` and on every `mcp[]`
entry with a `url`:

| field | default | what it does |
|---|---|---|
| `insecureTLS` | absent = verify | `true` sends the request without checking anything. What it buys is a connection; what it costs is that **any host between here and there can answer for that endpoint**, which is why the session says so in the log every time it starts using it. There is no "skip the certificate but still check the hostname": in `crypto/tls` those are one verification |
| `caFile` | absent = the system trust store | a PEM bundle to verify against, **and the only thing it is verified against**: naming a file is a trust decision, so the system store is not consulted beside it. The hostname is still checked |

Three things follow, and all three are checked where the config is written rather than on the first
request that would need them:

- the two together are refused: one says "do not check the certificate", the other says which one to check;
- a `caFile` that cannot be read, or that holds no `-----BEGIN CERTIFICATE-----`, is an **error** — never
  "fall back to the system store then". The endpoint that needs a `caFile` is precisely the one the system
  store cannot verify, so a fallback would turn a wrong path into a session that looks healthy over a
  connection nobody checks;
- a policy on an entry with no connection to verify (a `stdio` server) is refused as well, because a
  config line that does nothing is a line someone will later read as a guarantee.

`insecureTLS` keeps three states over the API: **absent** means unchanged, `false` means verify, `true`
means do not. And because a string cannot spell "no file", a body carrying `"insecureTLS": true` is taken
as the whole answer about how that endpoint is verified, and clears a stored `caFile`.

A policy belongs to the identity of the endpoint: changing it rebuilds the provider and reconnects the
server, while a patch that only spells out `false` — which is what a UI sends without being asked —
reconnects nothing. The transport a policy needs is `http.DefaultTransport` cloned with only the half the
policy names replaced, so trusting one CA cannot change anything else about how the endpoint is dialled — the
proxy from the environment surviving a certificate policy is the property a test checks, and so is a
certificate policy surviving a proxy. On `llm` neither TLS field has a control of its own in the settings
dialog: they are set in `data/<id>/config.json` or by `PATCH` (a dialog save keeps them, because it sends
back the fields it did not draw). On an `mcp[]` entry both have one — a CA file box and a switch — in the
form, beside the `url` and `headers` they apply to.

### Reaching an endpoint through a proxy

An endpoint that is only reachable by proxy needs the address written down, and it can be written in either
of two places — the same string in both: `socks5://`, `socks5h://`, `http://` or `https://`, with
`user:password@` in front when the proxy asks for one.

| where it is written | whose answer it is |
|---|---|
| `proxy`, at the session level (so also in `defaults`) | every endpoint in this session that does not name one |
| `proxy` on `llm`, or on an `mcp[]` entry with a `url` | that endpoint, and nobody else |

The nearer answer wins, and the inheritance is one hop deep:

| session | endpoint | what happens |
|---|---|---|
| set | — | the session's proxy |
| — | set | the endpoint's proxy |
| set | set | **the endpoint's** — the session's says nothing further about that endpoint |

Chaining two of them is not implemented and no order between them has been decided, which is why the rule is
"one answer per endpoint, nearest wins" instead of a list. One function reads the two levels
(`internal/agent/proxy.go`), so a version that wants a chain has one place to change and one table here to
rewrite.

Four details are what make the field usable, and the first three are settled before a config is saved rather
than on the request that would need them:

- **empty is not "direct"**. Empty means nobody said anything, which leaves `HTTP_PROXY`, `HTTPS_PROXY` and
  `NO_PROXY` in charge exactly as they were before this field existed. The word for "reach the endpoint
  itself, environment included" is `"direct"`, and it beats an inherited value — which is how one endpoint is
  taken off a proxy the rest of the session is on.
- **an address this program cannot parse is a refused config, not a direct connection.** A scheme outside the
  four, a missing host, a path or a query string on the end: refused, with the endpoint named in the message.
  Traffic meant to leave through a host somebody chose does not get to leave some other way because the
  address was typed wrong. Nothing is dialled while saving either: a proxy that is *down* is an outage worth
  reporting when it is needed, while a proxy that is misaddressed is a typo worth fixing now.
- **`NO_PROXY` stops applying to an endpoint that named a proxy**, `localhost` included. That endpoint said
  where its traffic goes, which is the question `NO_PROXY` exists to answer. Everything without a `proxy`
  field keeps the environment's rules, and a transport with a proxy in it is still `http.DefaultTransport`
  cloned with only its `Proxy` replaced — pooling, timeouts and HTTP/2 survive, exactly as with `caFile`.
- `socks5` and `socks5h` are both accepted and passed through as written: where a hostname is resolved for a
  SOCKS proxy is the standard library's doing, and rewriting one spelling into the other would be this
  program changing an address you typed.

Over the API a `proxy` behaves like `caFile`: **absent or empty means unchanged**, and no wire spelling means
"back to nothing said" — send `direct`, or take the key out of `data/<id>/config.json` to let the environment
decide again. It is part of an endpoint's identity, so changing it rebuilds the provider and reconnects the
server; changing the session-level one moves every endpoint that never named one, and leaves the ones that did
connected. A `proxy` on a `stdio` entry is refused like a `caFile` on it is, because there is no connection to
route.

One asymmetry with `llm.apiKey`, which the API blanks before answering: `GET` does echo a `proxy`,
credential and all, exactly as it echoes the `Authorization` header inside an `mcp[]` entry. The reason is
the PATCH rule above — an `mcp[]` list is replaced whole, so a field the server blanked would be dropped by
the next settings save that never mentioned it. The log line is where the credential is masked.

What is in force is logged once when a session opens and again when its endpoints change, with any password
in the address taken out:

```
llm: https://llama.internal:8080/v1 is reached through socks5://user:***@proxy.internal:1080
mcp server "vr": http://127.0.0.1:8200/mcp is connected directly — HTTP_PROXY and NO_PROXY are not consulted for it
```

The MCP form has a field for it; `llm` and the session-level answer are set in `data/<id>/config.json` or by
`PATCH`, as with the certificate fields.

### Tools: switches, prefixes and the cache

A tool is published under its **own name**: `fs_list`, not `mcp-vroot__fs_list`. Nothing is namespaced
unless you ask for it, because a tool whose name needs a prefix to be understood is a badly named tool.
Three things are configured **per session**, all in `mcp[]`:

| field | default | what it does |
|---|---|---|
| `disabled` | `false` | do not connect to this server at all (this used to be a dead key; the `mcp-` trick in a config is no longer needed) |
| `disabledTools` | none | hide these tools from the model; matches the **raw** name (`fs_list`) exactly or as a `path.Match` glob (`fs_*`, `*`), case sensitive |
| `toolPrefix` | none | namespace in front of this server's tools: `"vr"` ⇒ `vr_fs_list` (one underscore) |
| `structuredContent` | the session's own `structuredContent` | what this server's `structuredContent` is worth to the model: [one of four words](#structured-content-in-four-words) |
| `insecureTLS`, `caFile` | verify normally | how this server's `url` is trusted — the same two answers as on `llm` ([here](#reaching-an-endpoint-no-trust-store-knows)), and refused on a `stdio` entry, which has no certificate to check |
| `proxy` | the session's `proxy`, else what the environment says | how this server is reached — the same address as on `llm` ([here](#reaching-an-endpoint-through-a-proxy)), `direct` for no proxy at all, and refused on a `stdio` entry, which has no connection to route. It is part of the server's identity, so changing it reconnects |

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

### The daemon's own two tools

Two tools are answered by `bareloopd` itself: no server to start, no connection to keep, nothing to install.

| tool | what it does | what it takes |
|---|---|---|
| `wait` | pauses the loop and resumes it later. Nothing is sent to the model while it waits, and the result of this call is what the resumed turn reads | either `duration` (`"5m"`, `"1h30m"`, `"30s"`; a bare number is seconds) **or** `until` (an RFC3339 moment, e.g. `2026-11-01T22:25:00+08:00`). A moment more than a century away is refused as the typo it is, and an `until` written without an offset is read in the session's own timezone |
| `get_datetime` | the date, the clock, the offset, the zone it is naming, the day of the week — and an RFC3339 stamp on the last line, which is a `wait` argument waiting to be pasted | an optional `timezone`, for the case where the interesting question is what time it is somewhere else |

Both are **off until a session names them**, because they are handles on the loop rather than capabilities: a
builtin added in a later version must not turn up in a session that never asked for it, which is exactly what
an off-list would do. So `builtinTools` stores the names that are **on**, and the tools drawer, which sends
what is switched off for every group, has that translated at the one place that can do it. A name that is not
a builtin is refused at the API rather than ignored — a config line that silently does nothing reads later as
a guarantee — while a hand-written `data/<id>/config.json` gets a line in the log and is otherwise ignored.
The group is published in `GET .../tools` as the server `builtin`, and no MCP server may take that name: a
transcript saying `builtin/wait` has to mean one route. A builtin is published under its own name or not at
all — never renamed to make room, never prefixed, since the name is what the documentation quotes and what a
transcript already quotes.

`timezone` is what the two tools answer in when a call does not say. An IANA name is resolved against the
platform's zoneinfo and, where there is none, against the copy embedded in the binary, so a container with no
`/usr/share/zoneinfo` still knows `Asia/Taipei`. Nothing configured is **UTC** rather than the host's zone,
because which machine answered a request is not information anybody asked for; a session that wants the host's
zone writes `Local`. A zone that cannot be resolved is refused where `builtinTools` is checked, when the
config is written, rather than surfacing as a failed tool call an hour from now. Neither field has a control
of its own in the settings dialog: `timezone` is set in `data/<id>/config.json` or by `PATCH`, and
`builtinTools` is switched in the tools drawer.

`wait` is the one tool that can outlive the request that asked for it, and what the daemon holds while it
waits, what a restart keeps and what a restart drops, is [here](docs/design.md#sleeping-and-who-owns-the-wake-up).

### Structured content, in four words

A tool result can carry two channels: `content` (text, images) and `structuredContent` (JSON validated
against the tool's `outputSchema`). Both are stored in the log, **apart**: the entry's `content` holds the
text and its `structuredContent` field holds the raw JSON, exactly as it arrived. Nothing is deleted and
nothing is pasted twice, which is what lets the mode be changed afterwards — old entries re-decide on every
payload because assembling never rewrites the entry it read.

What the model is sent is one setting per session (`structuredContent`, next to `asm` rather than inside it,
because the per-server words live in `mcp[]`), overridable per server:

| word | the model sees | use it when |
|---|---|---|
| `auto` *(default)* | the text, plus the JSON **unless** the text already contains it | most servers. A server that already pretty-prints the JSON into its text block is not charged for it twice |
| `fallback` | the text, and the JSON **only** where there is no text | a server whose text is the prose answer and whose JSON is the same object for a machine |
| `always` | the text plus the JSON, every time | the JSON has keys the text leaves out, or the text is one status line |
| `drop` | the text only | a large `outputSchema` you never want tokenised: the history, the payload dump and the replay all lose it, the log does not |

The words are checked where they are written — a typo is refused at create and at `PATCH`, it does not
quietly become `auto`. `GET .../payload` shows which mode won for the next request.

Four words are a cache decision as much as a token decision. The fold happens at **assembly**, so the mode is
not recorded in the entry and changing it rewrites the text of every tool result the session already has: the
KV prefix is gone from the first one, and the next request computes the whole history again. The daemon says
so when it starts, and `inspect`/`payload` show the truth before the request goes out. A mode switched and
then switched back does not hand the prefix back, which is why the settings dialog offers one dropdown rather
than a per-turn choice.

How much of that JSON reaches the model is one more number per session — `structuredMaxBytes`, beside
the word and not inside the `asm` block — and it defaults to **16 384 bytes**, read on this project's ruler
of four bytes per token: a capped result is four thousand tokens on the same scale that decides
compaction and the pre-send fit check. It exists because the size of that field is chosen by somebody
else: one server answering a directory listing with ten megabytes of JSON spends a window on raw data,
and the session discovers that as a refusal. It is one number for the session and not one per server,
because a token budget is easier to hold in your head than a budget per server.

```
[structuredContent truncated: the first 16384 bytes of a 10485760-byte document are shown above, and it is
missing its last 10469376 bytes. What is above is cut off mid-document and will not parse; answer only from
what is shown, and say what you cannot see.]
```

What survives the cut is the **beginning**, because a prefix of the same JSON is byte-identical on every
rebuild of this session and costs the prompt cache nothing after the turn that first showed it — which is
why this is a head and not a head-and-tail clip like the one the summariser falls back to. The note goes
with it, worded as an instruction for the same reason the note about an unsent image is: a fragment that
reads like a document gets answered as one.

The ceiling is on the **wire only**. `data/<id>/log.jsonl`, `GET /api/session/{id}` and the transcript
keep the whole document, so raising the number later means a bigger payload rather than a repair — the
same property that makes the four words reversible. A ceiling has no floor to type past: one small enough to
show almost nothing is a decision about a chatty server — obvious in the first tool result and one PATCH from
fixed — while a negative is refused where it is written, because it is what people type for "no limit" and
would be stored as a number the session reads back as the default. What it does not cap is the tool's
`content` text: a server can spend a window with prose just as easily, and cutting a tool's own answer is a
different decision from deciding how much of its JSON to quote.

On screen the word is not applied: **both channels are always shown**. A tool result that carries the second
channel gets one more fold inside its card, labelled `structuredContent` and — once opened — how many
characters it holds. It is closed like the output fold above it, and its JSON is formatted and laid out only
on the first open, so a server that answers every call with 100 KB of JSON costs the transcript one 24 px row
until someone asks to read it. The four words and the ceiling are both set in the settings dialog, where
the cost of both is repeated in one sentence — each can be priced in a whole KV prefix rather than a
handful of tokens.

### Compaction, in short

A turn is folded away when `contextUsed > n_ctx × threshold%`: everything before the earliest boundary
that fits `(threshold% - keepTarget%)` of the window becomes one summary message. The kept tail is that
budget or the newest `minKeptMessages` messages, whichever is greater, rounded outward so the cut never
lands inside a tool span, and `minKeptUserTurns` — how many whole user turns it should prefer to hold —
is held when a boundary can hold it and given up by moving the cut inside a turn when none can. The old
branch stays in the log, so a compaction can be undone by moving the tip back. Failures fall open - the
worst case is a session that keeps more context, never one that loses it
([details](docs/design.md#compaction-in-one-paragraph)).

### Debugging what the model actually saw

```sh
curl -s localhost:8090/api/session/main/payload   # the exact body of the next request
BARELOOP_PAYLOAD_DUMP=1 ./bin/bareloopd ...        # and dump every request to the log
./bin/bareloopd -pprof 127.0.0.1:6060              # Go runtime profiles: heap, goroutine, CPU
```

`-pprof` is off unless you ask: `-pprof 127.0.0.1:6060` gets a listener of its own, `-pprof same` mounts
`/debug/pprof/` on the API port (the config file's `"pprof"` field takes either, and the flag wins).
Nothing authenticates any of it, and `/debug/pprof/profile` holds a connection for thirty seconds of CPU
sampling, so `same` on a non-loopback `-addr` earns a startup warning. It answers what no log in this
project can: what one session and its MCP servers cost in goroutines, whether an interrupted run left one
parked, and whether the process is still holding every `data/<id>/` transcript it opened at startup.
Nothing about the browser — that side has its own instruments.

Every turn also logs one line - `turn 3: 14 messages + system prompt (287 bytes), 9 tools, 41233 bytes`.
Language and script are not free: the assembler adds no prose of its own, so unless the system prompt says
otherwise the model answers in whatever it prefers
([why the system prompt is the only lever](docs/design.md#what-is-actually-sent)).

## HTTP API

| method | path | what it does |
|---|---|---|
| GET | `/api/health` | liveness |
| GET | `/api/sessions` | every session with its status, and what each one adds up to - how many entries its path holds, how many records its log holds, and what that log costs the disk. Answered from the index: listing sessions reads no transcript, and a session with no index is listed without those numbers rather than with zeroes. `lastTs` is the newest timestamp any entry of the log carries - the log's newest entry, not the entry the path is standing on, so moving a path backwards never makes it look fresh |
| POST | `/api/sessions` | create a session (body: a config; `id` is required, lowercase, and becomes the directory name - 400 without it, 409 if the directory exists) |
| GET | `/api/session/{id}` | config (without the API key), status, **the path** in path order, pending ids, and a `window` saying how much of it came back (see [paging the transcript](#paging-the-transcript)) |
| PATCH | `/api/session/{id}` | update the config; refuses while a run is active; a field the body does not carry is kept, not cleared. The name of a session is a config field, so this is also how it is renamed |
| DELETE | `/api/session/{id}` | close and delete the session |
| POST | `/api/session/{id}/unload` | close the session and forget it **without touching its directory**: the parsed conversation, the log handle and the MCP connections are given back to the process, and the next read of the id opens it again from disk (listing sessions has never needed one open). A session with a run or a summary in flight is refused with a 400 naming the phase — `/stop` first, then put it down. The body is the row this produced (`closed: true`) plus `unloaded`, which is `false` when this process was not holding the session at all: it is shut, which is what was asked for. What only memory had goes — a message held instead of written, and the in-memory half of a parked wait — the same bill a restart pays, and the wait itself is recomputed from the log on the next open |
| POST | `/api/session/{id}/send` | `{text, images[], files[], label}` - send now: the message is appended, or held in memory until the path is free again ([three moments](#three-moments-for-a-message)) |
| POST | `/api/session/{id}/interrupt` | `{text, images[], files[], label}` - abort the current generation - or end a wait that has been handed to the daemon - and send this message immediately after. **The body has to carry a message**, by exactly the rule `send` applies (whitespace alone is nothing; a picture is one), and both answer 400 `agent: empty message` - aborting without saying anything is what `/stop` is for. While a manual summary is running it is **refused** - a summary is not a generation to abort, so `/stop` ends it first and this works after |
| POST | `/api/session/{id}/stop` | abort what is in flight: a run keeps what it produced, a parked wait is ended (that is all there is to stop on a session that is asleep), and a manual summary is abandoned - it writes nothing until it finishes, so stopping one loses nothing |
| POST | `/api/session/{id}/resume` | continue from the current tip without appending anything; the path must end on a user message, a tool result or a compaction summary |
| POST | `/api/session/{id}/compact` | `{instruction}` - compact now, even below the threshold. The summary belongs to the session rather than to this request: disconnecting does not end it, and `/stop` is what ends it. While it runs, a `send` is held in memory and written behind the boundary it commits (which is what earns the next run), and an `interrupt` is refused |
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
| GET | `/api/session/{id}/turns` | one row per turn of the path — the id to jump to, its `kind`, its `ts` and the first line of its text — with `total`, and `hasOlder` when the ceiling meant older turns did not come back; `?limit=` takes fewer, from the newest end |
| GET | `/api/session/{id}/events` | the event stream as SSE (fallback transport) |
| GET | `/ws` | the event stream as a websocket (primary transport) |

### Paging the transcript

`GET /api/session/{id}` answers with the **path** — the entries that would be sent, oldest first — and
nothing else. An entry on a branch nobody is standing on is not part of any path, so it stays in the log
and in `/inspect` instead of crossing the wire to be thrown away.

| parameter | what it asks for |
|---|---|
| *nothing* | the whole path — the default on purpose: the cost panel prices a whole session with one unparameterised call |
| `?limit=40` | the newest page of at most 40 entries; 200 is the ceiling whatever you ask for, and `window.clamped` says when it was used |
| `?limit=40&before=<id>` | the page before that one — `before` is exclusive and is normally the `oldestId` just received |
| `?limit=40&after=<id>` | the page after that one — `after` is exclusive too and is normally the `newestId` just received: what comes back begins on the next turn, so nothing a reader holds is handed to it twice |
| `?anchor=<id>&limit=40` | a page centred on one entry, for a jump that lands far from both ends; an anchor is a place in a page, so it is asked for with a size |
| `?all=1` | the whole path by name, for inspecting and debugging |

`before` and `after` cut the path whether or not a `limit` came with them, because "everything older than
this entry" and "everything newer than it" are both questions with an answer. Past the tip `after` has
nothing left to give: an empty page comes back with `hasNewer` false. An id that is not on the path is a
400 either way — answering such a request with the whole path would be guessing at it.

`window` is always there: `oldestId`, `newestId`, `hasOlder`, `hasNewer`, `count`, `total`, `bytes`,
`limit` (what you asked for; 0 means no ceiling was asked for), `maxLimit`, `clamped`, `truncated`.

A page is a run of **whole turns**: it begins on a user message or on a compaction summary, so a tool
call never sits on one page with its result on the next, and walking `before` back from the newest page
reproduces the path with no gap and no overlap. The one exception is a single turn larger than the page
it was asked into: it comes back whole and `truncated` is true, because two pages holding half a tool
call each are two pages neither can draw. Walking `after` forward from the newest id of a page does the
same thing in the other direction, which is what lets a reader let go of what it has scrolled past and
ask for it again later. Combining any two of `before`, `after` and `anchor`, or `all` with anything else,
is a 400 — as is a `limit` that is not a positive integer, an `anchor` with no `limit`, or an id that is
not on the path.

`GET /api/session/{id}/turns` reads the same path at the resolution of a ruler instead of a page: one row
per turn (`id`, `kind`, `ts`, `snippet` — the first line of that entry, capped at 120 runes and marked
with an ellipsis where it was cut) and no content at all, 500 turns per answer. The ids it offers are
exactly the positions a page is allowed to begin at, which is what makes them worth jumping to: every row
is an `anchor` for the pager, so a jump cannot land between a tool call and its result. Asking for 500
turns of a longer session is not an error — the newest 500 come back, `clamped` is true and `hasOlder`
says the path goes on before the first row. `before`, `after`, `anchor` and `all` are 400s here, because
a turn
list that skipped a stretch of the path without saying so is worse than one that refuses.

### Websocket protocol

Text frames, JSON. The server sends one object per frame: `{type, session, seq, ts, ...}`, plus
`{type:"reply", req, data}` / `{type:"reply_error", req, error}` answers. The read limit is switched off,
because images travel as base64 inside one message.

Three commands come in, and none of them touches a session: `ping`, `subscribe` (`{session: "a,b"}`, empty
means all) and `snapshot` (replay what the hub is still holding for one session). **Every action is REST**, so
a button never waits on the event transport and `curl` can do everything the page can - the socket that
carries events is deliberately not a second way to run a session, because two decoders for one action is how a
frame ends up losing a field the other one kept. Anything else arriving on the socket is answered with an
error that says where to go instead.

If the websocket drops the UI does not give up on it: it retries with a 0.6 s to 15 s backoff forever and,
after a few failures, subscribes to SSE (or a 1.5 s poll) as a second channel, saying which of the two it is
living on and when the next socket attempt is due - a socket that comes back closes the fallback again. The
status line is the transport's own report, never a guess. Two tabs on one session stay in sync from these
events, and a tab re-reads the path when the tip moves to an entry it has never seen: another tab moved the
path or appended to it while this one was in the background.

Event types: `hello`, `entry`, `delta`, `tool_call`, `tool_result`, `state`, `pending`, `compaction`,
`request`, `request_done`, `log`, `info`, `error`, `removed`, `tools` (the tool set was rebuilt; the body is
the same view `GET .../tools` returns).

An `entry` frame for a reply that was streamed out carries `streamId` too — the id its `delta` frames came
on — so a client that has been drawing those deltas can finish *that* node with the committed entry (id chips
and token counts arriving, the streaming marker going) instead of drawing a second node for a message it
watched being written. Entries that were never streamed — a tool result, a message accepted while an answer
was on the wire, a compaction summary — omit the key. The entry arrives before the `delta` frame that closes
its stream, which is the other half of the promise: nothing closes a buffer before the entry that would
inherit it exists.

### Three moments for a message

A message can arrive at three moments, and they are not the same transaction.

**Nothing is owed.** The path ends somewhere a model can be asked again: the session is idle and not writing a
summary, or the last entry is a user message, a tool result, or a compaction summary. `POST .../send` appends
the message like any other entry, which makes it *pending* - on disk the moment it is accepted, counted by
`status.pending`, read at the next turn boundary: after the tool result belonging to the current turn, or as
soon as a compaction finishes. A restart cannot lose it.

**An answer is owed.** Two shapes of the same debt. A `tool_call` reached `data/<id>/log.jsonl` and its result
has not been written - while a slow tool runs, while a `wait` sleeps, while a parked session waits on a timer
with no goroutine held open for it. Or a request went to the model and has not come back: nothing is on disk
yet, so the entry that would answer it does not exist.

Appending into the first gap would put a user message between the call and its result, and the assembler answers
that shape by inventing a failure for a tool that is merely still waiting. Appending into the second would put
the message *before* the answer that was already being written: the path then ends on that answer, nothing is owed
any more, and the message is never read, because the answer beside it was written before it existed. So
`bareloopd` takes the message **into memory instead**: the request is accepted, `status.queued` counts it, no
entry is written, and no event names it. When the answer is finally written the held messages are appended behind
it, in the order they arrived, and the model reads them after it - and a message sent while that flush is running
either joins it ahead of the queue or lands after everything in it, never between two.

**Taken is not answered.** A run reads a message at a turn boundary, and a turn boundary is a moment the run
arrives at by itself, so accepting a message while a run owns the session makes a promise the session keeps
beside it: when that run ends, the path is looked at once more, and if it still ends on a message with no
answer, the run that answers it is started there. Without that promise, a message that arrived in the last
instant of a run — or one written out of the queue behind a summary the person stopped — sits on the path with
nobody left owing it anything. The one thing that cancels the promise is the stop button, and it cancels it
because stopping is the person answering the question of who owns the message.

**A summary is being written.** A manual `/compact` holds the path for a different reason: the entry it is
about to append *is the boundary*, and the plan for that boundary was made from the path as it was when the
summary started. Appending now would put the message **under** a summary that cannot know it exists — it is not
lost (the boundary records an entry id, not "everything before this entry"), but it would sit inside a region
nothing summarised, counted by a plan that never saw it. So it waits, counted by `status.queued` rather than
`status.pending`, and the moment the summary lands — or is stopped, which writes nothing — it is appended
behind the boundary, and the run that message earns is started for it. A summary the loop asked for itself is
not this moment: a run owns that one, and a message sent to it is held or appended by exactly the rule that
applied a moment before the summary started - the summary adds no rule of its own, because there is a run to
read whatever it leaves on the path.

**Stopping is not an answer.** `/stop` aborts the generation, writes what it had produced, and lets the held
messages join the path; it does not ask the model anything, and it spends any promise the session had made
about answering what it took. What is left is a question at the end of the path, and
the way to continue it is the button on that message
([run again from any legal break point](#using-it)), not a second copy of the same words. A message sent after
the stop is a new decision, and earns its answer like any other message.

Ending a wait early flushes what was waiting rather than leaving it behind. `/interrupt` ends it as itself, so
the model reads the wait saying `user interrupt` and then reads the message; `/stop` ends it too, and keeps
everything that was produced.

**A restart drops what was held.** The wait itself survives — its moment is recomputed from the tool call
already in the log, whether the process that died was sleeping it or holding it on a timer, so it wakes on
schedule whatever happened to the daemon — but the messages held in memory in front of it are written nowhere,
because nothing would be left to answer the call they queued behind. That
is the documented cost of holding them at all: the alternative is a path with a hole in it, and a hole is
exactly what the assembler has to invent an answer for
([sleeping, and who owns the wake-up](docs/design.md#sleeping-and-who-owns-the-wake-up)).

One box on screen shows both kinds, because a reader cannot tell them apart from the message itself:
`pending:` counts what is on the path, `held:` what is off it. A held message is quoted only where this tab
was the one that sent it; the rest are counted, not invented
([the UI, and why it is shaped like this](docs/design.md#the-ui-and-why-it-is-shaped-like-this)).

## Tests

`go test ./...` runs the unit tests of the packages that have them. The binaries in `cmd/llmmock` and
`cmd/mcpstub` exist so the whole loop - streaming, tool calls, usage accounting, compaction, abort - can be
exercised without external services.
