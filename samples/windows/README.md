# bareloop on Windows — file tools + browser, each in its own folder

Two ready-made configurations, no Docker:

| | config | file tools (`fs_*`) | browser writes | downloads | what keeps the browser honest |
|---|---|---|---|---|---|
| **shared folder** | `config.shared.json` | `C:/bareloop-workspace` | same folder | `C:/bareloop-workspace/Downloads` | `--filesystemRoot` = that folder |
| **IIS site** | `config.iis.json` | `fs_*` → `C:/bareloop-iis-site`<br>`art_*` → `C:/bareloop-browser` | `C:/bareloop-browser` only | `C:/bareloop-browser/Downloads` | `--filesystemRoot` = an artifacts folder **outside** the site |

The shared folder is the simplest story: one directory, both tool families see
everything, downloads included. The IIS config splits the world the way the
scenario demands — the agent edits the deployed site through `fs_*`, tests it
through `http://127.0.0.1:8081/` (real IIS, never `file://`), and everything the
browser emits (screenshots, downloads, temp, profile) is enforced — not
requested — to land outside the site folder, in `C:/bareloop-browser`, where
`art_*` can read it back.

## Paths: forward slashes, always

Every path in these files is written `C:/like/this`. Keep it that way when you
edit them. bareloopd parses this config as JSON, and in JSON a backslash is an
escape character — `"C:\\Users..."` needs doubling, `"C:\Users..."` corrupts the
file in a way that fails confusingly far from the cause. Chrome, Node, and Go
all accept forward slashes on Windows; use them everywhere.

## Install

```powershell
# 1. the two binaries (from the bareloop repo root)
go build -o samples/windows/bin/bareloopd.exe ./cmd/bareloopd
git clone https://github.com/cs8425/mcp-vroot ../mcp-vroot
go build -C ../mcp-vroot -o ../bareloop/samples/windows/bin/mcp-vroot.exe .

# 2. the browser MCP — project-local, nothing global (needs Node.js LTS).
#    From this directory: `npm i` installs the version pinned in package.json,
#    and the configs here already point at node_modules/.
cd samples/windows
npm i

# 3. Chrome — the config assumes C:/Program Files/Google/Chrome/Application/chrome.exe
```

## Fill in your paths (each appears exactly once per file)

| what | where |
|---|---|
| workspace / site / artifacts folder | `vroot.*.json` (`fs.path`) **and** the config: `--filesystemRoot`, `--userDataDir`, and the systemPrompt sentences |
| Chrome binary | `--executablePath` |
| chrome-devtools-mcp version | `package.json` (`npm i` afterwards); the entry `.js` itself is fixed at `node_modules/chrome-devtools-mcp/build/src/bin/chrome-devtools-mcp.js` — no path to fill |
| the LLM | `llm` block (baseUrl / apiKey / model) |
| IIS URL (iis config) | the systemPrompt's `http://127.0.0.1:8081/` |

Then create the folders and seed the download directory:

```powershell
mkdir C:\bareloop-workspace
# seed the Chrome profile so downloads land in the shared folder
# (iis mode: copy into C:\bareloop-browser\profile and edit the path inside
#  Default\Preferences to C:/bareloop-browser/Downloads first)
Copy-Item -Recurse profile-seed C:\bareloop-workspace\.browser\profile
```

Chrome merges `Default/Preferences` on first launch and keeps `download.default_directory`
— page downloads then land where your file tools can see them. The rest of the
profile (cookies, cache) stays inside the artifacts folder too, so nothing about
this browser lives in your own user profile.

## Run

```powershell
cd samples/windows
.\bin\bareloopd.exe -web -config config.shared.json   # or config.iis.json
```

Open <http://127.0.0.1:8090> and ask for something visible: *"build a page that
… — test it in the browser and screenshot the result."* For `config.iis.json`,
point IIS at the site folder first:

```powershell
New-Website -Name bareloop-demo -PhysicalPath C:\bareloop-iis-site -Url http://*:8081
```

- Writing into `C:\inetpub\...` needs rights — run bareloopd elevated, or grant
  yourself the folder (`icacls C:\bareloop-iis-site /grant "YOU:(OI)(CI)F"`).
  The app pool reads through inherited permissions.
- The agent never touches IIS itself: it edits files and verifies over http.

## What is enforced, and what is not

**Enforced by the tools:**

- every file the browser writes (screenshots with `filePath`, saved uploads) —
  outside `--filesystemRoot` the MCP refuses the call;
- every `fs_*` / `art_*` call — each mcp-vroot process is sandboxed at OS level
  (Go's `os.Root`), `..` cannot escape, per-folder rw/ro is config;
- file-chooser uploads read through the same whitelist.

**Not enforced (by design of the tools, handled by the prompt):**

- a screenshot with **no** `filePath` goes to the OS temp dir — both
  systemPrompts tell the agent to always pass an absolute `filePath`;
- downloads with an unseeded profile go to your user's Downloads folder —
  that is what the `profile-seed` above is for.

If you want temp files inside the artifacts folder as well, redirect the
temporary dir before starting bareloopd (child processes inherit it):

```powershell
mkdir C:\bareloop-browser\tmp
$env:TMP = 'C:/bareloop-browser/tmp'; $env:TEMP = $env:TMP
.\bin\bareloopd.exe -web -config config.iis.json
```

## Multiple folders = multiple vroot instances

The IIS config runs `mcp-vroot` **twice**: `fs_` for the site, `art_` for
artifacts, one tiny config file each. That is what `tool-prefix` exists for, and
it beats teaching one server several roots: the prefix *is* the boundary (each
tool family is teachable in one sentence of prompt), each process stays a
single-root sandbox you can audit in one line, and a stdio Go process is free.
A multi-root server would need a routing argument on every tool call — one more
thing for the model to get wrong per call, and one more traversal surface in the
code. Adding a third folder is a 4-line json plus six config lines:

```json
{ "name": "docs", "type": "stdio", "command": "bin/mcp-vroot.exe", "args": ["-c", "vroot.docs.json", "-stdio"] }
```

(`server-filesystem` from the MCP org supports many roots in one Node process;
it is fine too, but it buys nothing here and drops you out of the static-exe,
zero-runtime story.)

## Why chrome-devtools-mcp as the browser MCP

`chrome-cdp-cli` (`chrome-cdp mcp`) is a legitimate MCP server and has a real
advantage — one Go exe, no Node, and it drives *your already-running Chrome*
with your logins. It loses on exactly the requirement these samples are about:
path governance. Its policy covers origins and **upload** roots (`upload_roots`),
but `screenshot -o` writes to any path it is given, and page downloads go to
whatever your browser's own settings say — nothing about it can be aimed at a
folder. chrome-devtools-mcp inverts that posture: `--filesystemRoot` is a
whitelist, it launches its own browser, and that browser's profile (downloads
included) is a folder you seeded.

### Appendix — variants that drive your real browser

- **chrome-cdp-cli**: download `chrome-cdp.exe`, then
  `chrome.exe --remote-debugging-port=9222`, put it on a tab and
  `chrome-cdp policy init` (MCP mode refuses to start without an allow list),
  add `upload_roots` to the printed policy. Config entry:

  ```json
  { "name": "chrome", "type": "stdio", "command": "chrome-cdp.exe", "args": ["mcp"] }
  ```

  Accept the gaps above (screenshots unrooted, downloads to your Downloads) or
  treat it as a human-supervised tool, not the sandboxed one.
- **The fork (`cs8425/chrome-cdp-cli` `mcp-enhance`)** adds
  `--disable-safety-policies`, `--include-structured-content`, `--http`,
  `--cors` — quality-of-life for other clients, no path features: the first
  flag switches *off* policy (upload roots included), and the
  structuredContent workaround is not needed for bareloop, which parses
  `structuredContent` natively (`internal/mcp/client.go`: `CallResult.Structured`,
  assembler fallback per `asm.StructuredFallback`), while `--http` answers a
  transport need bareloop does not have (stdio).
- **Middle ground**: launch a dedicated Chrome
  (`chrome.exe --remote-debugging-port=9222 --user-data-dir=C:/bareloop-browser/attached`)
  and let chrome-devtools-mcp attach with `--browserUrl http://127.0.0.1:9222`.
  File writes are still whitelisted by `--filesystemRoot` (they happen in the
  MCP process), and downloads follow the attached profile's seeded Preferences.

## Troubleshooting

- **Why `node` + the entry `.js` under `node_modules/`** — not `npm i -g`
  (no reason to touch the machine's global state for one sample; the version
  is pinned right here in `package.json`) and not `npx chrome-devtools-mcp`
  (the form the upstream README shows). `npx` costs three things a daemon
  should not pay. One: every start goes through npx's cache check, and an
  unfetched version reaches the network. Two, worse: on Windows `npx` is a
  `.cmd` shim, so the process is a chain — `cmd.exe → npx → node (server) →
  Chrome` — and closing a session kills only the process bareloopd spawned,
  itself blunt (`Process.Kill()` — one process, no tree); the inner server and
  its browser survive as orphans with pipes nobody reads. The same orphaning
  applies to any `.cmd` shim (`chrome-devtools-mcp.cmd`, bare `npx`, bare
  `chrome-devtools-mcp`) as the command. `node` + the entry `.js` is exactly
  what those shims exec — one process, killed cleanly, started instantly.
  Three: a config file should name a *pinned* browser MCP, not `@latest`-shaped
  drift. If you insist on npx anyway (and accept the orphan chain when a
  session closes): `"command": "cmd.exe"` with args
  `["/c", "npx", "-y", "chrome-devtools-mcp@1.12.0", <the flags above>]`
  — `cmd.exe` because bareloopd spawns directly, and a bare `npx` name would
  hit the shim problem one level up.
- **`Unknown arguments` on start**: the installed version predates the flags —
  bump `package.json` and `npm i`. Flags worth remembering: chrome args
  go through `--chromeArg=<arg>` (`--customChromiumArgs` was never released);
  `--filesystemRoot` may repeat.
- **screenshots "refused"**: the `filePath` left `--filesystemRoot`, or the
  agent sent a relative path — both correct refusals; the prompt teaches
  absolute paths.
- **stderr about falling back to the 2025-11-25 handshake**: normal; bareloop
  negotiates down and the connection is up.
