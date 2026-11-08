# bareloop-browser — bareloopd + chrome-devtools-mcp + Chromium

One image, one port, one data directory: an agent that can open and drive a real browser.
The MCP server is [chrome-devtools-mcp](https://github.com/ChromeDevTools/chrome-devtools-mcp),
running as a stdio subprocess inside the container; the Chromium it drives is installed in
the same image. Nothing leaves the container except the LLM calls.

## Build

The base image must exist first — this Dockerfile copies the binary out of it:

```sh
docker build -t bareloop .                      # the base, context = repo root
docker build -t bareloop-browser samples/browser   # this sample, context = its own folder
```

(From inside this directory the second line is just `docker build -t bareloop-browser .`;
its `.dockerignore` keeps the context down to the config file — your profile and data
sitting next door never enter the build.)

Pin the MCP server version when you want reproducible rebuilds:

```sh
docker build -t bareloop-browser --build-arg CHROME_DEVTOOLS_MCP_VERSION=1.12.0 samples/browser
```

## Run

```sh
docker build -t bareloop ../..    # once: the base image (its context is the repo root)
docker compose up --build      # first run also builds; afterwards: docker compose up
```

The compose file mounts `./data` (kept in git), runs as uid 1000 by default — the owner of
a fresh clone (on another machine, put `BARELOOP_UID=$(id -u)` and `BARELOOP_GID=$(id -g)`
in a `.env` next to it) — and takes the LLM from `BARELOOP_BASE_URL` / `BARELOOP_API_KEY` /
`BARELOOP_MODEL` (shell or `.env`, defaulting to a llama.cpp on the host).

### Without compose

```sh
docker run --rm -p 8090:8090 --shm-size=1g --user "$(id -u):$(id -g)" \
  -v "$PWD/data:/app/data" \
  -v "$PWD/profile:/profile" \
  -v "$PWD/artifacts:/artifacts" \
  -e HOME=/artifacts -e TMPDIR=/artifacts \
  -e BARELOOP_BASE_URL=http://host.docker.internal:9931/v1 \
  -e BARELOOP_API_KEY=none \
  -e BARELOOP_MODEL=qwen3.6-27b \
  --add-host=host.docker.internal:host-gateway \
  bareloop-browser
```

- `BARELOOP_BASE_URL` / `BARELOOP_API_KEY` / `BARELOOP_MODEL` are expanded into the baked
  config at startup — the LLM lives outside the container (llama.cpp on the host in the
  example above), and the key never enters an image or a file.
- `HOME` / `TMPDIR` are what pull the browser's downloads and default screenshots out of
  the container — see [Artifacts](#artifacts-screenshots-and-downloads); omit them and
  those files stay inside, gone with the container.
- `--shm-size=1g` gives Chromium shared memory; the config also passes
  `--chromeArg=--disable-dev-shm-usage` so it survives a small `/dev/shm` regardless.
- `--add-host=host.docker.internal:host-gateway` is how Linux containers reach a host port;
  Docker Desktop has it already.
- `--user "$(id -u):$(id -g)"` runs the agent as you instead of the image's default uid
  65534, so bind-mounted data stays yours — switchable at run time, no rebuild, because
  `/app` is world-writable.
- Drop `--rm` and the `-v` and the session vanishes with the container — which is sometimes
  exactly what you want from an agent holding a browser.
- A mounted `./config.json` (`-v ...:/app/config.json:ro`) replaces the baked
  one wholesale — sessions, tools, all of it.

Open `http://localhost:8090` and try:

> Check the performance of https://developers.chrome.com

or, for something quieter:

> Open https://example.com and take a screenshot of the page.

The browser process starts lazily — connecting to the MCP server does not open a window;
the first tool that needs a page does.

## The profile: fresh or continuing, your call

The config points Chromium at `--userDataDir /profile`, and compose bind-mounts `./profile`
— so the browser's memory (cookies, local storage, cache, logged-in sites) lives on your
host, in plain files, outside every container lifecycle:

- **Keep it** (the default): the agent stays signed in across runs; test a flow that starts
  half-done.
- **Fresh browser, guaranteed**: stop the container, `rm -rf profile/*`, start again.
- **A different session per experiment**: stop, move `profile/` aside, start (a new one is
  built), swap directories to switch identities.
- Want the old throwaway behaviour back: `--isolated` in place of `--userDataDir` in
  `config.json` — temporary profile, gone with every MCP server restart.

One container at a time per profile directory — Chromium locks its user-data-dir.

## Headed: watch it browse

`config.headed.json` + `compose.headed.yaml` are the on-screen variant — Chromium opens
windows on your X11 desktop (Wayland works via XWayland):

```sh
docker compose -f compose.yaml -f compose.headed.yaml up --build
```

The window is for you; the agent drives through CDP and never clicks pixels, which is
exactly why headless stays the default. Use headed when you want to watch a reproduction,
show a colleague, or eyeball a page the agent finds hostile to automation.

## GPU

Unnecessary for automation, and turned off: chrome-devtools-mcp (via puppeteer) passes
`--disable-gpu` by default, and page rendering is done by software (SwiftShader — kept
switched on for WebGL sites with `--chromeArg=--enable-unsafe-swiftshader`, without which
newer headless Chromium simply fails WebGL).

For actual GPU — WebGL-heavy pages, performance traces that mean something — three pieces
have to line up, so it is a recipe rather than a flag:

1. **Re-enable what puppeteer disabled**, in the MCP args: `--ignoreDefaultChromeArg=--disable-gpu`
   (this is a chrome-devtools-mcp flag, not a `--chromeArg`).
2. **Give the container the render node**, and permission for it:
   ```yaml
   services:
     browser:
       devices:
         - "/dev/dri:/dev/dri"
       # and whichever render group your distro uses, e.g.:
       group_add:
         - "render"
   ```
   (NVIDIA needs `--gpus all` + nvidia-container-toolkit instead of /dev/dri — more moving
   parts; AMD/Intel via Mesa are the easy path.)
3. **The in-container driver**: add `mesa-vulkan-drivers` to the image's apt list (Vulkan
   ICDs), then point Chromium at it with `--chromeArg=--use-angle=vulkan`. Verify inside
   the running browser at `chrome://gpu` — it should stop saying "Software only".

Expect a real difference only on GPU-bound pages; screenshots, form-filling and navigation
run the same either way.

## Artifacts: screenshots and downloads

Everything the browser emits lands in `./artifacts` on your host, mounted at `/artifacts`:

- **Screenshots the agent saves**: `config.json` sets `--filesystemRoot /artifacts`, the
  directory chrome-devtools-mcp lets its file-writing tools touch (default: the OS temp
  directory — the `/tmp/chrome-devtools-mcp-XXXXXX/` paths you may have seen elsewhere).
  `filePath` accepts absolute paths or paths relative to the *MCP server's* cwd, which is
  `/app` here — so the agent must pass absolute `/artifacts/...` paths; the system prompt
  says so.
- **Default screenshots** (no `filePath`): those always go to a fresh `mkdtemp` under
  Node's temp root, which ignores `--filesystemRoot` — so the compose file sets
  `TMPDIR=/artifacts` and they land there too.
- **Downloads**: Chromium has no "download directory" flag; its default is `$HOME/Downloads`.
  The compose file sets `HOME=/artifacts`, so downloads appear at `./artifacts/Downloads`.
  Both env vars are inherited by the MCP child processes; nothing else here reads them.

The filesystem sample goes one better: its `--filesystemRoot` is the same directory the
file tools serve, so a screenshot the agent takes is immediately readable as a project file.

### Screenshot sizes

Headless Chromium has no window, so `--viewport 1920x1080` is baked into the config —
without it the page area follows the host screen and screenshots arrive at sizes nobody
chose. Two sizes it does *not* control, by design:

- **`fullPage` screenshots** grow to the page's full content height (that is the point) —
  a 1920x1080 viewport can still produce a 1920x6000 image.
- If the images eat too much model context, cap what comes back with
  `--screenshotMaxWidth 1280` / `--screenshotMaxHeight 1280` (server flags, downscale
  before the image is returned; image tokens scale with pixels, not bytes). The headed
  variant instead sizes the window with `--chromeArg=--window-size=1280,900` — there the
  window is the viewport, so no `--viewport` flag.

## What's in the box

| Piece | Choice | Notes |
|---|---|---|
| Base | `node:26-trixie-slim` | chrome-devtools-mcp needs Node LTS |
| Browser | Debian `chromium` + Noto CJK/emoji fonts | see caveat below |
| MCP server | `npm i -g chrome-devtools-mcp` | global, so no npm at run time |
| Launch flags | `--headless --viewport 1920x1080 --userDataDir /profile --executablePath /usr/bin/chromium --filesystemRoot /artifacts --chromeArg=--no-sandbox --chromeArg=--disable-dev-shm-usage --chromeArg=--enable-unsafe-swiftshader` | fixed 1920x1080 viewport (headless has no window; without it the viewport is the host screen — screenshots then carry odd sizes); persistent profile + artifact root, both bind-mounted (see above); `--no-sandbox` because Chromium's setuid helper cannot work in a container. Note the flag is `--chromeArg=` in v1.12 (the repo's `main` docs call it `--customChromiumArgs`; that name predates the release and yargs rejects it) |
| Telemetry | off | `--no-usage-statistics`, `--no-performance-crux`, plus the `CHROME_DEVTOOLS_MCP_NO_*` env vars |

Caveat: chrome-devtools-mcp officially supports Google Chrome and Chrome for Testing;
Chromium "may work". It does here, but if you hit an oddity, swap the apt package for a
Chrome for Testing install and point `--executablePath` at it — nothing else changes.

## Files

- `Dockerfile` — the image
- `compose.yaml` — `docker compose up --build` and it runs (data in `./data`, profile in `./profile`, artifacts in `./artifacts`)
- `compose.headed.yaml` — overlay that shows the browser on your X11 desktop
- `config.json` — the baked config (edit, rebuild, or mount your own over it)
- `config.headed.json` — the no-`--headless` variant, shadow-mounted by the overlay
