# personal-agent-win — the whole agent in one folder (Windows, no Docker)

One folder, three `.exe` files, two `.bat` double-clicks: an agent with a real Chrome
window and a file sandbox — the **same folder** seen from both sides. No containers,
no installer, nothing written outside the folder.

Unlike [`../windows/`](../windows/README.md) (which is about path *governance* across
two folders), this sample is the single-shared-workspace layout in its smallest
copyable form: give it to a colleague as a zip.

## Layout

```
personal-agent-win/
├── start-chrome.bat   starts the Chrome the agent drives (CDP :9223, own profile)
├── start-agent.bat    starts bareloopd (UI on http://127.0.0.1:8090)
├── config.json        the bareloop config — the llm block is yours to edit
├── vroot.json         fs MCP sandbox: jail root = workspace, read-write
├── bin/               the three executables go here        (gitignored)
├── profile/           the browser's memory: cookies, logins (gitignored)
├── data/              sessions                              (gitignored)
└── workspace/         the agent's whole world               (gitignored)
    └── Downloads/     shared upload/download folder
```

## 1 — Copy the folder, drop in the executables

Copy this entire folder anywhere. It expects exactly three files in `bin\`:
`bareloopd.exe`, `chrome-mcp.exe`, `mcp-vroot.exe`. From this repo's root — with the
two MCP repos cloned one level up as siblings:

```bat
go build -o samples\personal-agent-win\bin\bareloopd.exe .\cmd\bareloopd
git clone https://github.com/cs8425/chrome-mcp-go ..\chrome-mcp-go
go build -C ..\chrome-mcp-go -o ..\bareloop\samples\personal-agent-win\bin\chrome-mcp.exe .
git clone https://github.com/cs8425/mcp-vroot ..\mcp-vroot
go build -C ..\mcp-vroot -o ..\bareloop\samples\personal-agent-win\bin\mcp-vroot.exe .
```

Neither MCP repo ships pre-built release binaries yet — GitHub Actions is planned —
so until artifacts exist, building from source is the only path. When releases land,
"how they get there" becomes a download like any other: a CI artifact, a different
machine, a zip. `bin\` only knows three file names.

Every path in `config.json` and `vroot.json` is relative, and both bats compute their
own location (`%~dp0`), so the folder works at any path on any drive. The bats are CRLF
via `.gitattributes` and also survive a zip that flattens them to LF — no labels, no
multi-line blocks, every line stands alone.

## 2 — Point it at an LLM

Edit only the `llm` block of `config.json`: `baseUrl`, `apiKey`, `model`. If the
OpenAI-compatible endpoint is a llama.cpp on the same machine, the defaults already fit.

## 3 — Run the two bats

1. **`start-chrome.bat`** — opens the dedicated Chrome: profile in `profile\` next to
   the bat, debug port 9223. It is the agent's browser; keep the window open.
2. **`start-agent.bat`** — starts bareloopd on `config.json`. The bat also stamps
   `workspace\HERE.txt` with the workspace's absolute path (see below).

Order is free — the browser MCP attaches lazily, on the first `cdp_*` call. Open
<http://127.0.0.1:8090> and ask:

> Build a page that … — test it in the browser and screenshot the result.

## One file, three names

The fs tools' virtual `/` **is** `workspace\` on disk, and the browser shares parts of
it by path mapping (`-root` / `-browser-root` / `-browser-vroot` in the config):

| virtual (the agent's view) | physical | notes |
|---|---|---|
| `/index.html` | `workspace\index.html` | fs_* and chrome-mcp see the same bytes |
| `/downloads/x.bin` | `workspace\Downloads\x.bin` | `cdp_download` captures land here; `cdp_upload` reads only from here |
| `/shot.png` (saveAs) | `workspace\shot.png` | screenshots written with `saveAs` are ordinary workspace files |
| `/HERE.txt` | `workspace\HERE.txt` | rewritten at every `start-agent.bat` run: the workspace's absolute path, so the agent can name its own pages as `file:///` URLs |

## ⚠ Warnings — read before letting it drive

**The layout is for a supervised demo, not an unattended box.**

- **The port is unauthenticated** (like every sample here): anything that can reach
  `127.0.0.1:8090` can drive the agent — and through it, Chrome. Bind loopback only,
  which the config does.
- **The file jail governs `fs_*` calls — not the browser.** `mcp-vroot` sandboxes its
  own tool calls at the OS level, but Chrome runs with your user's file permissions,
  and the agent holds CDP. In particular the model can still **read and write files
  outside the workspace**:
  - **`file://` reads** — `cdp_tabs nav` to `file:///C:/Users/you/Documents/notes.txt`
    and `cdp_read` the content. No special tool needed; navigation is a first-class
    tool, and this sample's own preview pattern (`file:///` pages) is the door it
    walks through.
  - **`cdp_raw_call` writes (and reads)** — every CDP method, ungated:
    `Page.captureScreenshot` with a `filePath` anywhere Chrome can write,
    `Browser.setDownloadBehavior` pointed anywhere, `IO.read` for anything already
    open. raw_call is the widest door in the box.

  Narrow it in `config.json` at the cost of features:

  ```json
  "disabledTools": ["window", "raw_call"]
  ```

  That closes raw CDP (writes, exotic reads) while keeping the guided tools — but the
  `file://` **read** door stays open as long as the agent may navigate at all. Closing
  that fully means taking `file:///` previews away (serve pages over a local http
  server instead) — at which point the workspace jail and the browser finally share
  the same fence line.
- **`profile\` is the agent's memory too**: whatever it gets signed into while you
  watch stays in that folder, in plain files. Delete the folder for a blank identity.
