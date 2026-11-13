# bareloop

A server-side agent loop with context compaction and a web UI. Everything a model can do comes from MCP
servers - the program ships **no built-in tools**.

It is meant to sit next to a local model: the core is deliberately nothing but the loop and the tool
loop's bookkeeping, and it talks to llama.cpp, vLLM, Ollama or LM Studio through their OpenAI-compatible
endpoint.

## What it is like

- **A short list of dependencies, all pure Go, deliberately**: the standard library,
  `github.com/coder/websocket` for the browser transport, `github.com/ncruces/go-sqlite3`
  (SQLite compiled to WebAssembly — no CGO, no system SQLite, no C toolchain anywhere) and
  `github.com/klauspost/compress` (zstd, including its dictionary trainer). No framework, no
  build step for the UI, and a machine that only runs the compiled binary installs nothing.
- **One SQLite file per session, and nothing else.** The conversation is a tree: entries are
  rows, the edges are rows, superseded revisions keep their own table, and a `meta` row says
  which node is current. Moving the tip is time travel: an abandoned path stays in the tree.
  WAL journalling makes a torn commit a rollback, not a repaired file. Payloads compress per
  row — zstd with a trained dictionary by default — and a row's own bytes say which codec
  wrote it, so every store reads every store
  ([`docs/zstd-dictionary.md`](docs/zstd-dictionary.md)). One RAM note with the default:
  zstd's high-compression encoder keeps roughly 20 MB of match tables resident once the
  first row big enough to compress (at or above the 512-byte floor) has been written —
  the library would size them per CPU core (~10 MB each), but
  the pool depth is pinned at two slots (`-zstd-slots`; `0` hands it back to the core
  count), so the figure is flat whatever the machine has
  (measured on the heap profile; it is the encoder, the decoder stays lazy). That is the
  encoder's appetite, not compression per se — gzip's pooled writers cost a few hundred
  KB each — so run with `-compress gzip` if even 20 MB matters more than the ratio:
  rows record the codec that wrote them, so any store still reads either way. It is a tree, but the UI shows one
  path at a time: what is not on the path is not drawn, and there is no branch switcher yet
  (`docs/design.md`, Deferred).
- **The loop runs on the server**, not in the browser, so a run survives a page reload and survives
  closing the tab.
- **Nothing is hidden from you.** `inspect` shows the token count of every message and the exact numbers
  the compaction decisions were made from, `payload` shows the literal body of the next request, and every
  request leaves one line in the process log.
- **Context compaction that catches up instead of choking.** When the conversation runs several times
  past the trigger it is compacted one tipping point at a time: every round summarises up to the message
  that tipped the window over, commits its own entry, and bills its own calls; a message too large to fit
  on its own is read at its beginning and its end and the summariser is told the middle was omitted.
- **The window is discovered, not assumed**: the effective `n_ctx` is the smaller of your ceiling and what
  the server reports, and a session that outgrows the window is repaired instead of stuck.
- **Multi-session**: every session has its own log, MCP server set, system prompt, sampling parameters,
  context window and compaction parameters.
- MCP **2026-07-28** and **2025-11-25**, over stdio and streamable HTTP, with a dialog in the UI whenever a
  tool needs input.
- Context compaction follows the contract in
  [`docs/compact-logic.md`](docs/compact-logic.md).

| read this | for |
|---|---|
| this file | what it is, how to build it, how to use it |
| [`docs/design.md`](docs/design.md) | how it works and why: the context window, compaction invariants, prompt cache, retries, MCP eras, the UI's shape, the markdown renderer's hooks, and what is deliberately deferred |
| [`docs/zstd-dictionary.md`](docs/zstd-dictionary.md) | the storage codec and its dictionary: what a payload row is made of, how the shipped zstd book is trained, how to train your own, and what losing one costs |
| [`docs/api.md`](docs/api.md) | the full HTTP and websocket reference: every endpoint, every query parameter, every event frame — the contract a client relies on, so this README stays prose |
| [`docs/compact-logic.md`](docs/compact-logic.md) | the compaction logic in diagrams: the data model, the three cut shapes, the two request shapes, replay, commit, the invariants — the authority when the prose docs disagree (implementation-agnostic: a harness can be built from it alone) |
| [`config.example.json`](config.example.json) | every config field, documented inline |

## Build

```sh
go mod tidy
go build ./...
```

Go 1.27 or newer is what `go.mod` asks for ([why](docs/design.md#go-version)).

The daemon takes no memory flags of its own — it is a Go program, so the runtime's
`GOMEMLIMIT` and `GOGC` apply as they do everywhere else; on a small machine, a
`GOMEMLIMIT` of a few hundred MB turns a transient peak into a GC hurry-up instead
of a resident high-water mark.

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

`-testdata <dir>` is the same idea for files that are not the UI: it serves that directory at `/testdata/`,
and none of it is compiled into the binary. That is how a page that is still being written gets a URL, and
how such a page fetches a fixture — a page opened from disk can import neither a module script nor a JSON
file. It is off unless you name it, a file that is not there is a 404 rather than the single page, and
nothing in it is cached. Naming the directory `bareloopd` runs from is refused, because that is where
`config.json` and `data/` are, and static paths — `/testdata/` among them — are what the login gate
does not cover (`keeping the daemon behind a login`). `testdata/bench-stream.html`
lives there today.

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

Every session directory holds `config.json` and `store.db` — the SQLite file that is the tree,
and the only file that has to survive (plus its `-wal` sidecar while a daemon has it open; a clean
close checkpoints it away, so a resting directory is one file again). The session list is answered
from each store's summary rows, so a session nothing has opened still appears in it with its
numbers ([what is on disk, and why](docs/design.md#what-is-on-disk) — written for the JSONL log of
the `log-text-based` branch; on this branch the answer is one SQLite file).

## Configuration

`config.example.json` documents every field. `defaults` is inherited by every session; each session stores
its own copy in `data/<id>/config.json`.

| field | meaning |
|---|---|
| `addr`, `data`, `web`, `fsync`, `debug` | listen address, log directory, UI directory (empty — the default — is the copy compiled into the binary), synchronous appends, request logging |
| `defaults.llm` | `kind` (`openai`), `baseUrl`, `apiKey`, `model`, `nCtx`, `headers`, `pricing` (`input` for the prompt tokens the server had to compute, `cachedInput` for the ones it served from cache, `output` for what it generated — **all three per 1 000 000 tokens**, and no currency field, because bareloop cannot know which one you are billed in. Leave it out and nothing is priced; an explicit `0` is an answer meaning free. A session inherits it and may override; a price changes no request, so saving one never reconnects anything) |
| `defaults.systemPrompt` | the system prompt: the one place that decides language and script |
| `defaults.mcp[]` | `name`, `type` (`stdio`/`http`), `command`/`args`/`env`/`envPassthrough`/`envBase`/`dir`, `url`/`headers` |
| `insecureTLS`, `caFile` | on `defaults.llm` and on any `mcp[]` entry with a `url`: how an https endpoint is trusted ([reaching an endpoint no trust store knows](#reaching-an-endpoint-no-trust-store-knows)) |
| `proxy` | at three levels — `defaults.proxy` (every session's), `llm.proxy`, `mcp[].proxy`: the nearer answer wins, and empty leaves the environment in charge ([reaching an endpoint through a proxy](#reaching-an-endpoint-through-a-proxy)) |
| `defaults.builtinTools` | the tools the daemon answers itself, named one at a time: `wait`, `get_datetime`. It is an **on-list**, so all of them are off until a session says otherwise ([the daemon's own two tools](#the-daemons-own-two-tools)) |
| `defaults.timezone` | the zone the time tools answer in when a call names none: an IANA name (`Asia/Taipei`), a fixed offset (`+08:00`, `UTC+8`), `Local` for the machine's own - and nothing at all means UTC ([the daemon's own two tools](#the-daemons-own-two-tools)) |
| `defaults.compact` | compaction parameters (see below): `auto` is off by default, `inLoop` is **on** by default |
| `defaults.asm` | payload options: `sendReasoning` (**on** by default), `sendImages` (**on**), `omitToolResultName` (off) |
| `defaults.temperature`, `maxTokens`, `maxTurns`, `reasoningEffort`, `extra` | sampling defaults (see below) |
| `sessions[]` | optional sessions to create at startup |
| `mcpLock` | freezes every session's MCP server set over the API: sessions may enable, disable and re-policy the servers of `defaults.mcp` and nothing else about them ([locking the server set](#locking-the-server-set)) |
| `bind` | how the listener presents itself: `tls.cert`/`tls.key` (paths — serve https, and the startup line says so), `trustedProxies` (CIDRs whose `X-Forwarded-For` the login gate may believe about who a client was) ([behind a login](#keeping-the-daemon-behind-a-login)) |
| `auth` | one operator login in front of everything that acts: `user`, `pass` (env-expanded, so the file can hold `${VAR}`), `session.idleTimeout`, `rateLimit.maxAttempts`/`window` ([behind a login](#keeping-the-daemon-behind-a-login)) |

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
| `compact.summaryMode` | absent = `prefix-stable` (the summary request is cut from the payload bytes the loop sent, so the provider reads its own cache; a region those bytes cannot carry goes out as one flat transcript, oversized messages clipped and marked) | `"transcript"` asks with no cache affinity — always the flat shape; any other word means `prefix-stable` (the old chain modes `"iterative"` and `"map-reduce"` are gone: replay replaced the chain) |
| `compact.threshold` `summaryCap` `minSummarizeTokens` | `0`, negative or absent = the default (70 / 70 / 2000) — 0 here would be a budget of nothing, which is not an answer anyone means | `> 0`: percentages of `n_ctx`, and a token count for `minSummarizeTokens`. `summaryCap` is the summary's one number: max_tokens of its request and the limit its answer is graded against (the old `keepTarget` and `summaryMax` keys, folded into one; neither is read anymore) |
| `compact.minKeptUserTurns` `minKeptMessages` | **`0` is a real answer = no floor**, and an omitted key decodes to that 0 — so the 4 that a *new* session starts with is not reachable by leaving the key out | `> 0` is the floor; a negative clamps to 0, never to the shipped default |
| `compact.keepBudget` | absent = 20 (the tail's share of `n_ctx` — set apart from `summaryCap` so tightening the summary no longer resizes the tail); **an explicit `0` is a real answer = keep nothing but the summary**, the same floor rule as the two above | `> 0`: the percentage of `n_ctx` the kept tail may occupy; a negative clamps to 0 |
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

### The environment a stdio server starts with

A stdio server is third-party code, and the daemon's own environment holds the keys that `config.json` expanded with
`${VAR}` — that being the whole point of writing it that way: keys live in the environment, never on disk. So the
child gets no wholesale inheritance; its environment is built from three layers:

| layer | field | what the child sees |
|---|---|---|
| base set | — | `PATH`, `HOME`, `LOGNAME`, `SHELL`, `TERM`, `USER` on unix; eighteen launch-critical names on Windows. The same list the MCP TypeScript and Python SDK clients inherit: what a process needs to start, nothing that can carry a secret. |
| passthrough | `envPassthrough` | named variables whose **value** is read from the daemon's environment as the process starts. Only the names enter `config.json` and only the names come back from the API; the secret itself never lands on disk. |
| literal | `env` | `KEY=value` lines, last of all: they override a name the base set or a passthrough brought, and carry any value that exists nowhere else. |

`envBase: false` drops the base set too — that server starts with an empty environment plus the other two layers,
no `PATH`, no `HOME`. A name the daemon does not have is skipped: passing a value through requires one to be behind
the name. All three fields are part of the server's identity — the child's environment is part of what the process
is — so changing any of them restarts it.

Before this policy `env` *added to* a full inheritance of the daemon's environment, and writing no `env` still
inherited all of it. A server that relied on a shell variable needs an explicit line now — its name in
`envPassthrough`, or a `${VAR}` line in `env` — and an operator who wants the child to inherit less than even the
base set says so with `envBase`.

### Locking the server set

An MCP server is arbitrary code on your own machine — stdio spawns it, http reaches it — so *which servers exist*
is a machine decision, while *which of them a session uses* is a session decision. `"mcpLock": true` in
`config.json` is the line between them, and it is drawn where the writing happens:

| | under the lock |
|---|---|
| enabling and disabling a pre-registered server (`disabled`), its tool switches, its prefix, its structuredContent | **still the session's** — none of it is part of the server's identity, and none of it started a process or opened a socket it was not already connected to |
| `PATCH`, create and import carrying a *different* server list | **403**, naming what is still yours to do. An empty list is a list, and it is refused the same way |
| a body that says nothing about `mcp` | not a change, exactly as it was before the lock — the settings page keeps re-saving the whole config it read |
| sessions already on disk when the lock went up | **left exactly as they are**. The lock guards writes; it does not audit history |
| `config.json` itself — including its `sessions[]` lines | **open**: it is the trust root the lock is defined in, and a lock that refused the operator's own file would be locking its own house |

The comparison is itemwise identity — everything about an entry that would mean reconnecting it — with only the
enabled bit ignored, which keeps the lock and the reconnect logic agreeing forever about what a server *is*:
a re-saved list passes, a moved `url`, a changed `command`, a renamed or reordered entry is a new server set and
gets refused. The way back to a session's server set is the one for every other hand edit: stop the daemon, edit
`data/<id>/config.json`, start again. What the lock promises is narrow and total: nothing arriving over the API
can change which MCP servers this machine runs.

### Keeping the daemon behind a login

`auth` puts one operator login in front of everything that acts: the API under `/api/`, the event
transports (`/ws`, SSE), and `/debug/` when it is mounted. The static page itself and
`GET /api/health` answer without it — the first so the UI can load far enough to ask for the
password, the second so a probe stays probeable.

| field | default | what it is |
|---|---|---|
| `user`, `pass` | — | the one account; the block does nothing unless both are set. `${VAR}` expansion works here as everywhere in the config, which is how the password stays out of the file: `"pass": "${BARELOOP_AUTH_PASS}"` |
| `session.idleTimeout` | `168h` | how long a login cookie lives; each authenticated request slides it forward, so an open tab never asks again |
| `rateLimit.maxAttempts` · `window` | `5` · `15m` | wrong passwords one client may spend per window; past it every answer is `429` with a `Retry-After` until the window closes |

Two credentials are accepted: the cookie the UI receives from `POST /api/login`, and HTTP Basic
sent **pre-emptively** — `curl -u`, `go tool pprof -header`. Basic arriving because the server
asked is a different animal, and this server never asks: **a 401 carries no `WWW-Authenticate`,
ever.** A browser challenge pops its own dialog over this UI's login form, and worse, registers
the origin in the browser's HTTP auth cache, where one remembered wrong pair answers every later
request and poisons every `fetch` from the origin. A 401 here says 401 and means the JSON beside
it; the UI sees the number and shows its own form. ([design.md](docs/design.md#the-login-gate))

Wrong passwords are counted per client, and "client" is the **TLS fingerprint of the connection**
when there is TLS and the IP otherwise — one laptop reconnecting onto a new address cannot walk
past a lockout it earned there. Every login answer, right or wrong, is held to a constant
half-second, so timing says nothing about which half was wrong; blocked attempts are not charged,
and a success clears the slate for that client. A refused login is a bare error envelope,
`{"error": "authentication required"}` — the endpoint never says which half failed.

A login over plain HTTP to a machine you do not control sends the password in the clear, so with
auth on, a non-loopback `addr` and no `bind.tls` prints a warning at startup. A self-signed pair
is one command:

```
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 \
  -keyout key.pem -out cert.pem -days 365 -nodes -subj "/CN=localhost"
```

Browsers need the warning dismissed once per origin; over TLS the session cookie carries
`Secure`, and the login dialog the UI raises remembers the user across reloads until the idle
timeout expires or the logout button is pressed. `X-Forwarded-For` is consulted only for
addresses listed in `bind.trustedProxies` — an unasked reverse proxy cannot hand the gate a
forged client, and behind one that is not declared, all traffic shares one fingerprint bucket.

One transport footnote worth knowing before filing a bug: Go's HTTP/2 has no extended CONNECT
(RFC 8441), so a browser that negotiated h2 opens a parallel HTTP/1.1 connection for the
websocket. It is normal, and it authenticates with the same cookie like any other request.

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
that fits `keepBudget%` of the window becomes one summary message. The kept tail is that
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
Nothing authenticates a listener of its own, and `/debug/pprof/profile` holds a connection for thirty seconds of CPU
sampling, so `same` on a non-loopback `-addr` earns a startup warning — though `same` is the one mount a
configured `auth` covers: `/debug/` is behind the gate, so it wants the credential pre-emptively
(`go tool pprof -header Authorization: Basic …`). It answers what no log in this
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
| GET | `/api/health` | liveness: `{ok, time}` |
| GET | `/api/sessions` | every session as a list row — path, log and disk figures and all — answered from the index: listing sessions reads no transcript and opens nothing |
| POST | `/api/sessions` | create a session (body: a config; `id` is required and becomes the directory name — 400 without it, 409 if the directory exists) |
| GET | `/api/session/{id}` | config (without the API key), status, **a page of the path**, pending ids, and a `window` saying how much of it came back; `?config=1` answers config and status alone, `?usage=1` the whole path as a usage skeleton |
| PATCH | `/api/session/{id}` | update the config; refuses while a run is active; a field the body does not carry is kept, not cleared — the name is a config field too, so this is how a session is renamed |
| DELETE | `/api/session/{id}` | close and delete the session and its directory |
| POST | `/api/session/{id}/unload` | put the session down without touching its directory; a run or a summary in flight is refused — `/stop` first |
| POST | `/api/session/{id}/flags` | pin or archive a session without opening it |
| POST | `/api/session/{id}/send` | `{text, images[], files[], label}` — send now: appended, or held until the path is free ([three moments](#three-moments-for-a-message)) |
| POST | `/api/session/{id}/interrupt` | abort what is in flight — a generation or a handed-over wait — and send this message immediately after; aborting without saying anything is what `/stop` is for |
| POST | `/api/session/{id}/stop` | abort what is in flight: a run keeps what it produced, a wait is ended, a manual summary is abandoned |
| POST | `/api/session/{id}/resume` | continue from the current tip without appending anything |
| POST | `/api/session/{id}/compact` | `{instruction}` — compact now, even below the threshold; `/stop` ends it, disconnecting does not |
| POST | `/api/session/{id}/gate` · `/elicit` | answer a gate card (`accept`/`retry`/`skip`) or a tool's input request |
| POST | `/api/session/{id}/entry/patch` · `/entry/delete` | rewrite one entry's content in place (every path through it changes, and it does not branch), or delete an entry |
| POST | `/api/session/{id}/tip` | move the tip (time travel) |
| POST | `/api/session/{id}/reload` | re-read the log from disk |
| GET | `/api/session/{id}/inspect` | what would be sent: the system head, per-message token sizes, the plan, the tool list |
| GET | `/api/session/{id}/payload` | the exact body of the next request: `{system, messages, tools}` |
| GET · POST | `/api/session/{id}/tools` | the tools drawer — every server and tool with cost and use; the POST sets one server's policy, landed at the end of the run |
| GET | `/api/session/{id}/turns` | one row per turn of the path — each id a legal jump target for the pager — and no content at all |
| GET | `/api/session/{id}/tree` | the whole tree as shape only — every edge, every child count, and where the live path stands — with no content by construction; opening lines ride `/snippets?ids=` for exactly the rows on screen |
| GET | `/api/session/{id}/events` · `/ws` | the event stream as SSE (fallback transport) / as a websocket (primary) |

The two promises this table rests on are worth memorising; the parameters, the `window` fields,
the refused combinations and every event frame are in **[`docs/api.md`](docs/api.md)** — which also
carries the three endpoints a configured `auth` adds (`login`, `logout`, `whoami`) and what a `401`
means there ([behind a login](#keeping-the-daemon-behind-a-login)).

**The transcript is paged and cannot be talked out of it.** `GET /api/session/{id}` answers with
the **path** — the entries that would be sent, oldest first — and no request shape buys the whole
of it: the largest answer is **one page** (40 by default, 200 the ceiling), cut on whole turns so
a tool call never sits on one page with its result on the next. Silence buys a page, and so does
a bare `before` or `after` — one session's unparameterised read measured 73 MB once, and the
request that made it was "no query string". `?usage=1` is the one whole-path answer left: the
path as a skeleton of usage rows, content absent by construction rather than by flag, because a
bill is bounded by arithmetic and a transcript is not.

**Actions are REST, always.** The event socket carries frames out and three transport verbs in
(`ping`, `subscribe`, `snapshot`) — every action is a `POST`, so a button never waits on the
event transport, `curl` can do everything the page can, and one action has exactly one decoder.
Every frame carries a `seq` from the daemon's counter and the socket beats a `heartbeat` every
~20 s, so a page can tell "nothing happened" from "something happened that I never received";
after a drop the honest move is the one the shipped UI makes — reconnect, replay what the hub
still holds, re-read what it does not.

### Three moments for a message

A message can arrive at three moments, and they are not the same transaction.

**Nothing is owed.** The path ends somewhere a model can be asked again: the session is idle and not writing a
summary, or the last entry is a user message, a tool result, or a compaction summary. `POST .../send` appends
the message like any other entry, which makes it *pending* - on disk the moment it is accepted, counted by
`status.pending`, read at the next turn boundary: after the tool result belonging to the current turn, or as
soon as a compaction finishes. A restart cannot lose it.

**An answer is owed.** Two shapes of the same debt. A `tool_call` reached the session log and its result
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
