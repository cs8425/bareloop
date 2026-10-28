# How bareloop works

[README.md](../README.md) covers what the program is, how to build it and how to drive it. This is the
other half: the decisions, the invariants they leave in the code, and the concrete failures each rule
came from. Read it before changing the loop, the assembler or compaction.

Compaction has a contract of its own -
[`notes/compact-threshold-mode.en.md`](../notes/compact-threshold-mode.en.md) - and where this file
summarises it, that file is the authority.

## Layout

| path | what lives there |
|---|---|
| `internal/ir` | the entry tree: entries, kinds, roles, usage, the path from the tip, effective (post-compaction) entries |
| `internal/store` | the append-only JSONL log: entries, the tip, patches, deletes, session metadata, replay |
| `internal/asm` | the **only** place that turns entries into a provider payload, plus per-message sizes |
| `internal/llm` | one OpenAI-compatible provider with usage parsing for OpenAI, llama.cpp and Ollama |
| `internal/mcp` | JSON-RPC 2.0, stdio and streamable-HTTP transports, both protocol eras |
| `internal/compact` | compaction: parameters, triggers, the cut, the summary request, the gate, the serial plan |
| `internal/agent` | sessions, the manager, the loop, tool execution, compaction commits, `n_ctx` resolution |
| `internal/server` | the JSON API, the websocket stream, the SSE fallback, the static UI |
| `web` | the UI: HTML, CSS, ES modules, no build step |
| `cmd/bareloopd` | the server |
| `cmd/llmmock` | a fake OpenAI-compatible server, for testing |
| `cmd/mcpstub` | a fake MCP server (both eras, both transports), for testing |

Two boundaries are load-bearing: only `internal/asm` turns entries into a request (so `payload` is the real
request, not a re-derivation of it), and only `internal/ir` knows what the current path is (so compaction
never deletes anything - it appends and moves the tip).

## Session identity

The session **id is its directory name**: `data/test-1/` *is* session `test-1`, and the `"id"` field
inside `data/test-1/config.json` is a copy of that name, nothing more. Whenever a session is loaded
the two are compared, and a file that disagrees is reported and rewritten:

```text
session "test-1": config.json says id "test"; the directory name wins, rewriting config.json
```

Nothing else is allowed to re-home a session - the log path, the session list, `PATCH` and `DELETE`
all work from the directory. The reason is concrete: a directory whose config carried another
session's id became that session, so messages typed into one were appended to the other's log while
its own `log.jsonl` was never read.

An id is a directory name, so it is validated on the way in: lowercase `a-z 0-9 . _ -`, at most 64
characters, no `..`. Lowercase is not cosmetics: directory names are case-insensitive on Windows and
macOS, so with case folded away two sessions cannot differ only in how they are spelled, which is what
lets the filesystem itself guarantee uniqueness. `POST /api/sessions` requires an id and answers **400**
without one (it used to invent `s<timestamp>`, which left a session nobody asked for in the data
directory); creating over an existing directory is a **409**, and `DELETE` refuses an id that is not a
valid name. The last gate is `filepath.IsLocal`, the standard library's own answer to "is this a name a
directory can have": on Windows it rejects `con`, `nul`, `com1` and trailing dots, and everywhere it
rejects separators. Its rules belong to the running system, so a `data/` prepared on Linux can still
hold a name that Windows would refuse. The API still refuses to guess, but the UI will: it suggests an
id (`quiet-otter-1f3`) when the dialog opens, offers another on request, folds what you type into
lowercase, and makes one up if you clear the field - because the person pressing create asked for it, in
front of them. The id is not the label you see either: `name` is free-form, so spaces and non-ASCII text
belong there.

## The context window

`llm.nCtx` is a **ceiling you set, not a fact about the server**. The effective window is the smaller of
the two numbers that exist:

| config | server | used | why |
|---|---|---|---|
| not set | reports 40960 | 40960 | the server knows best |
| 16384 | 40960 | 16384 | you are deliberately spending less of the window |
| 262144 | 40960 | 40960 | more than the server has cannot be honoured: it would only be refused |
| 262144 | silent | 262144 | nothing to ask |
| not set | silent | unknown | compaction and the fit check stay off until you fill it in (Settings → `n_ctx`) |

The server is asked at exactly two moments: once before a session sends anything, and once when the
provider refused a request for being too large (which may mean the window was made smaller while the
session was idle). Probing on a timer would move the window under a session that is merely between two
turns for a reason nobody can act on. `/props`, `/model` and `/models` are tried in that order (llama.cpp,
vLLM, Ollama and LM Studio publish it under one of `n_ctx`, `max_context`, `context_length`,
`max_model_len`). **A probed number is never written back to config**, so moving to a server with a
different window needs no config edit; `status.nCtx` is the number in use, and `nCtxConfig` /
`nCtxSource` say where it came from.

A session that outgrows the window is repaired rather than stuck: the provider refuses the request, the
window is probed again, the oversized region is folded by the serial chain, and the run stops with
`the context was compacted, please send again` - the next message is answered normally. **Refusing one
request is the accepted design**, not a bug to pre-empt: until a request has been measured or refused
there is no real number to split on, and splitting on a character estimate would compact a session the
provider would have accepted. Nothing is guessed while `n_ctx` is unknown. The pre-send fit check
therefore trusts a real measurement and otherwise only refuses what could not possibly be answered
(more than 98% of the whole window).

Two consequences worth knowing when you read the code: `force` overrides a *measurement* - a forced
compaction ignores a trigger number that is known to be stale - but never a *policy*, so `compact.auto:
false` and `compact.inLoop: false` still refuse and say what to do instead; and right after a compaction
`status.contextUsed` reports `0` / `contextKnown: false` until the next successful turn, because there is
no measurement yet and pretending otherwise would drive the trigger off a guess.

## Summarising serially

When the conversation is too large to hand to the summariser in one request - which is also what happens
after one oversized message, since the summary request is bound by the same window - the summary is
produced **serially**: the region is cut into parts that each fit, and each call folds the next part into
the summary of everything before it (`cmp1 + part 2 -> cmp2`, and so on). A message too large to fit even
on its own is read at its beginning and its end, and the summary is told that its middle was omitted, so
that it does not invent one. Set `compact.summaryMode: "iterative"` to use the chain even when everything
would fit. `"map-reduce"` (summarise the parts in parallel, then merge) is reserved for that future
implementation and currently runs the serial chain - deliberately, because on a local model parallel
parts queue on the same GPU instead of helping.

The arithmetic is deliberately unfashionable: one part gets

```
summaryCap = n_ctx × keepTarget% / 100
fit        = n_ctx - summaryCap - summaryCap/2 - 1200    // output room, growth room, instruction
```

so a part plus its own reply can never overflow the window the way a single request holding the whole
conversation did (observed on a 40960 window with `keepTarget: 20`: `summaryCap` 8192, `fit` 27472, a
194,359-token region folded in three serial parts). Every cut lands on a legal boundary (a `user` or
`assistant` message, never a tool call or a tool result), and the part boundaries are chosen from the
measured per-message sizes, not from a character count. Which mode actually ran is written into the
compaction entry's meta, because the log line is not the durable record.

## Compaction in one paragraph

The path is the truth and exactly one assembler decides what leaves the process. A trigger is
`contextUsed > n_ctx × threshold%`, where `contextUsed` is the reported usage of the last completed turn
plus a token count of everything appended since. The cut keeps the newest suffix that fits `keepTarget%`
of the window, never inside a tool span and never before a tool result, and never fewer than
`minKeptUserTurns` user turns. Everything above it is summarised by a request whose prefix is
byte-identical to the last payload actually sent, so the provider's prompt cache survives. The commit
writes the boundary entry and moves the tip in one log record; the old branch stays intact, so any
compaction can be undone by moving the tip back. Acceptance is size-based (with one retry and then a
three-option gate), failures fall open, and the summary enters the payload as one `user` message with a
fixed marker prefix. `Inspect` shows the exact numbers the decisions were made from.

## What is actually sent

`GET /api/session/{id}/inspect` answers the size question (per-message tokens, budget, plan, tool list)
and also returns the `system` head verbatim; `GET /api/session/{id}/payload` answers the content question
with the exact body of the next request - `{system, messages, tools}`, the same `[]Message` values the
provider receives. The inspect panel shows both (`payload as sent`), so no part of the prompt is hidden
behind a byte count.

Every request also leaves one line in the process log:

```
turn 3: 14 messages + system prompt (287 bytes), 9 tools, 41233 bytes
```

and `BARELOOP_PAYLOAD_DUMP=1` appends the whole body as JSON after it - large, and only worth turning on
while debugging. Use it whenever the model behaves oddly: it settles in one line whether something other
than the user's own words is steering the answer.

Language and script are not free. The assembler sends no text of its own (there is not a single non-ASCII
string in the Go code or the UI), so the model answers in whatever it was trained to prefer unless the
system prompt says otherwise - which is how a Traditional Chinese question comes back in Simplified.
`defaults.systemPrompt` in `config.example.json` pins both: same language, same script, no unrequested
switching.

## When a request fails

A provider error is retried at most twice, and only where a retry can plausibly help: a transport failure
(the request never got an answer), 429, and the 5xx family. A 400/401/403/413/422 is an answer about the
request itself and will be identical forever, so it is not retried at all - except that a 400 saying the
request does not fit the window starts the recovery above.

Either way, **the turn ends with an entry**. If nothing ever came back, the loop commits an assistant
message that says so -

    [generation failed: llm: 500 Internal Server Error: {"error": …}]

- and the UI draws it as a failure rather than as an answer. That commit is not decoration: the loop
owes an answer exactly while the path ends with a user or a tool message, so committing nothing leaves it
owing one forever. A provider that answers 500 reliably then produces a storm - one identical request per
round, each with a payload line and an error event, forever. As the belt to those braces, a round that
committed nothing stops the run instead of starting another round.

## Images

An attached image is stored in the log as base64 and goes on the wire as an `image_url` part with the
message's own text next to it: an attachment does not replace what you typed, and a tool result keeps its
text beside its screenshot.

Whether a server can see at all is one request away: llama.cpp's `/props` answers with
`{"modalities": {"vision": true, …}}`, and `false` there means no projector is loaded. A text-only server
- a plain GGUF with nothing attached - has to parse the part anyway, and the base64 is charged to the
context whether anyone looks at it or not. For those, `"asm": {"sendImages": false}`: the payload then
carries one line instead of the bytes,

    [attached image4.png (image/png, ~184 KB) is in this conversation but its pixels were NOT
    sent to you: this session sends text only. You cannot see it, so do not describe it and do
    not guess what it shows - say that you cannot see the image and ask the user …]

which is worded as an instruction because the polite version does not work: a note that only mentions that
a picture was in the conversation gets answered with an invention. In a real session a model produced a
confident four-point description of an image it had never received, and its own reasoning channel said
why - "the prompt says the image was not sent … but since I am a multimodal model, I can actually see the
image". If `sendImages` is off, the only honest answer is "I cannot see it".

The UI always shows the real image, and an attachment stored as a reference rather than as bytes is shown
as that reference: a path is more use than a broken image icon.

## MCP notes

`2026-07-28` is stateless: no `initialize`, every request carries its identity in `params._meta`,
`server/discover` is the only discovery probe, `resultType` marks a final result, and multi round-trip
requests (a tool that needs input) replace server-initiated requests. The client probes `server/discover`
and falls back to the `2025-11-25` `initialize` handshake when a server does not know it.
`x-mcp-header` schema properties are mirrored into `Mcp-Param-*` HTTP headers, and an answered input
request is retried under a **new** JSON-RPC id.

A tool call whose server asks for input raises an elicitation request in the UI (the same dialog serves
legacy server-initiated `elicitation/create` requests).

## The UI, and why it is shaped like this

`web/js/session.js` and its neighbours are plain ES modules; three behaviours are worth knowing because
each of them was a bug first.

- **Order comes from entry ids.** Entries are stored in a map and served by `GET /api/session/{id}`; the
  handler sorts them by their numeric id and the UI sorts again, so a reload renders the same sequence
  that was produced. `status.tip` marks the current path, and **only the path is drawn**: an entry that is
  not on it belongs to a branch nobody can switch to yet, so it is not shown at all (it stays in the log).
  Sorting by id means two branches would **interleave** in one vertical list anyway (an id is creation
  order, not depth), which is the real reason there is no branch switcher: a tree cannot be drawn honestly
  by appending to a list.
- **The action row is copy / raw / run again / delete** - on a compaction entry, "run again" reads "re-make
  this summary" (see *Running the loop again*). `const BRANCH_UI = false` in
  `web/js/transcript.js` removes the tip-move button (see *Deferred*); `RESTART_UI = true` keeps "run again
  from here", which is a recovery tool rather than a way of browsing. There is no edit button: `entry/patch`
  rewrites shared history in place, which is not a thing to hide behind a pencil.
- **Caching is bounded, and cheap to revalidate.** `index.html` - the file that names every asset - is
  `Cache-Control: no-cache, must-revalidate`, so navigating to the UI always asks the server. Everything
  else is `public, max-age=60, must-revalidate`. Both carry an `ETag`, so a revalidation costs a 304 and
  no body, and a hard refresh always bypasses the lot. Editing `web/` while the server runs therefore
  shows up on the next navigation (or immediately on reload).
- **Following is opt-out, and the composer never loses.** The transcript scrolls with the output only
  while you are at the bottom; scrolling up (or wheeling up) unpins immediately and a floating
  `↓ latest` counts what you missed. A wall of tokens must never steal the viewport from the composer,
  because that is where stop and interrupt live. `#log` is the only scrolling container
  (`min-height: 0` on the flex items is what keeps the document itself from scrolling).

Below 860px the UI is one column: the session list becomes a drawer (☰, closed by the scrim, by picking a
session or by Escape) and the side panel becomes a bottom sheet. Heights use `dvh` because on a phone
`100vh` is the *largest* viewport and would hide the composer - the composer is where stop and interrupt
live - behind the browser's own bar; inputs are never smaller than 16px because iOS Safari zooms the page
otherwise; and padding adds `env(safe-area-inset-*)` so the notch and the gesture bar do not cover
anything.

### Running the loop again

`POST /api/session/{id}/resume` starts a run from the current tip and appends nothing. It exists because the
loop can stop half way - `Stop()` keeps the partial answer, a provider error ends the run, a forced
compaction ends it with "the context was compacted, please send again" - and the answer to all three is "ask
again with what you have". `Send` cannot do this job: it always appends a user entry and refuses an empty
message, so using it would write a duplicate question just to restart a turn.

Only three kinds of entry may end a request, so only those are offered as a break point: a **user message**
(answer it again), a **tool result** (the model thinks with the result it already has; the tool is *not*
re-run), and a **compaction summary** - which is not a request but the record of one, so its button does
something else, described below. A finished assistant answer is not offered, because "continue from your own
last answer" is a different question and answering it silently would be presumptuous. A half-finished or
failed answer is not a boundary either - its *parent* is - so its button moves the tip to the parent and the
truncated text is what gets given up.

Giving that up is what the confirm box is for: it counts the entries after the chosen one and says plainly
that they stay in the log but cannot be reached from the UI. When the tip already sits at the break point
with nothing after it - the ordinary case right after an abort - there is nothing to confirm and the run
just starts.

**A summary is re-made, not re-asked.** "Run again" on a compaction entry moves the path to the summary's
*parent* and asks for a summary of it (`POST /compact` with no instruction - the trigger writes the prompt it
would write today). That is precisely the state the compaction ran in: the path is the one it saw, and this
summary no longer applies to it. So the second summary is made from the same messages, and the old one plus
everything that had followed it leave the path.

Whether the run then continues follows the rule the loop already lives by - **what was running keeps running,
what had stopped stays stopped** - and the entry's label is the only trace of which it was. `in_loop` is
written before sending, inside a run (it is also what the forced compaction after a provider refused the
payload writes), so that one resumes; `end_of_turn` follows a finished turn and `manual` is the button pressed
while idle, so those two only replace the summary and leave the session stopped - starting a turn nobody asked
for is worse than a summary you have to follow up yourself. A summary that is still the tip has nothing to
redo: there the same label decides whether the button exists at all, and resuming an `in_loop` one is the
"the context was compacted, please send again" moment.

`Resume` deliberately does not call `needsRun()`: that function also drives the loop's own
auto-continuation, where a compaction boundary must say no (otherwise a compaction with nothing new pending
would immediately re-ask). For a resume the compaction boundary is the whole point.

### Embedding the UI

The UI is plain files, served from `-web` (default `./web`, looked up in a few usual places so
`go run ./cmd/bareloopd` works from the repo root). Shipping a single binary means replacing that
directory with an embedded filesystem - `//go:embed all:web` plus `http.FS`, one import in `cmd` and one
field in `server.Server`:

- keep the split: the entry point is revalidated, assets have a lifetime;
- the `ETag` is the only thing that has to change meaning - from "size + modification time" to a hash of
  the bytes, since embedded files all share the build time;
- if the asset names ever grow content hashes (`app.4f9c1.js`), drop `max-age` to a year and hash the
  name into `index.html` instead; until then 60s is the staleness budget that lets an upgrade self-heal.

## Markdown rendering

`web/js/md.js` is a dependency-free renderer that escapes first and only then builds markup, so pasted
model output cannot inject script. It exports `{ register, render, renderTo, escape }` and also sets
`window.bareloopMarkdown`, which is the extension point:

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

Hooks run in registration order; the first `code` hook that returns a string wins. A `script` tag of
`type="module"` is deferred, so an extension module placed before `app.js` still sees the renderer; a
classic inline script would run too early.

## Deferred

Built, half-built, and deliberately not built - with what each one is waiting for.

**Branching UI.** The log is a tree and `POST /api/session/{id}/tip` moves the path tip; the transcript has
no button for it - "branch here" sits in `web/js/transcript.js` behind `const BRANCH_UI = false`, and
flipping that constant is the whole re-enable, handler included. It is off because one list in id order
cannot show two branches side by side (they interleave), so a button that moved the tip would leave you
typing at the end of a path you cannot see, cannot name, and cannot get back from. "Run again from here"
stays available: it is not browsing, it is how a stalled loop is picked up, and it says which entries it is
giving up before it does it. What a tree view has to provide first:

- every child of an entry reachable from that entry (a fork you cannot find is not a feature, it is an
  orphaned subtree);
- which node is the tip, always visible, one click to move it there;
- what would be sent from that node before you move - `pathOf` already computes it client-side;
- and the paths that "run again from here" gives up have to come back into view: today they are not drawn at
  all, which is honest only while nothing can switch to them.

Off-path entries are not drawn at all, and `POST /tip` still works for anyone who wants it by hand: the
storage model did not change, and a log written by anything else may already branch.

**`entry/patch` does not branch.** `PatchEntry` rewrites one entry and replay keeps its `ParentID`
(`patches never move a node`, `internal/store/store.go`), so editing an ancestor changes the shared history
of every path through it. That is either exactly what you meant or quietly destructive, and the alternative
is a new entry with the same parent plus a tip move - a real fork where the old text survives on the old
path. Unresolved because "edit" is not in the UI either; the API table now documents the real behaviour.

**Not pre-splitting a request that will not fit.** On a freshly started server nothing has been measured
yet, so the first oversized request is *refused with a 400*, probed, and answered by a forced compaction
that asks you to send again. Guessing the window from config to avoid that one refusal was considered and
rejected: an accepted refusal is honest, a guessed window is a new wrong number to maintain. Revisit only if
the provider starts reporting its limit (then probe it at startup instead).

**`map-reduce` summary mode** is accepted by config and runs the serial chain, saying so in the log
(`internal/compact/iterative.go`). It is the answer to a region so large that serial latency dominates: the
parts are independent, so they can be summarised in parallel and merged.

**`CompactResult` does not report `summaryMode`.** The mode that actually ran is recorded on the compaction
entry - which is what replay and the inspector read - but the result of a manual compact still omits it.

## Go version

Go 1.27 or newer - that is what `go.mod` asks for, and an older toolchain refuses to build. The code
itself needs much less (the HTTP routes use the method-and-wildcard patterns added in 1.22), so raising
the requirement costs nothing and buys the 1.27 standard library: `encoding/json` is backed by the v2
implementation there, and `filepath.IsLocal` is what keeps session ids from being named `con` on Windows.
