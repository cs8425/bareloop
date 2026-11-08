# bareloop-webdev — an agent that writes the page and then opens it

The browser sample plus a filesystem MCP server rooted at `/workspace`: the agent edits
files and drives Chromium against the very same directory —
`file:///workspace/index.html` is the page it just wrote. Nothing else needs mounting;
bind-mount your project over `/workspace` and its session data wherever you like.

## Build

```sh
docker build -t bareloop .                         # the base, context = repo root
docker build -t bareloop-webdev samples/webdev     # this sample, context = its own folder
```

(From inside this directory the second line is just `docker build -t bareloop-webdev .`;
its `.dockerignore` keeps the context down to the config file — workspace, profile and
data sitting next door never enter the build.)

## Run

```sh
docker build -t bareloop ../..    # once: the base image (its context is the repo root)
docker compose up --build      # first run also builds; afterwards: docker compose up
```

Open `http://localhost:8090` and send:

> Restyle /workspace/index.html with a dark theme, then open it at 375px wide and screenshot it.

`./workspace` is the project — it ships with a placeholder page (committed to git, mounted
into the container as `/workspace`), so that prompt has something to look at on a fresh
clone; replace its contents with your own site whenever you like. The container runs as
uid 1000 by default so it writes those files as you — the owner of a fresh clone (another
machine: `BARELOOP_UID=$(id -u)`, `BARELOOP_GID=$(id -g)` in a `.env` next to the compose
file).

## One filesystem, two tool families

The browser and the file tools share `/workspace` — the config sets chrome-devtools-mcp's
`--filesystemRoot` to exactly the directory the filesystem server serves, and the compose
file points two environment variables at it so even Chromium's own notions of "my
folders" land inside:

| What the browser emits | Where it lands (host) |
|---|---|
| Screenshot saved by the agent (`filePath`) | anywhere under `./workspace/…` — pass absolute `/workspace/...` paths; relative ones resolve against the MCP server's cwd (`/app`) and get refused |
| Default screenshot (no `filePath` — the tool mkdtemp's under Node's temp root) | `./workspace/artifacts/chrome-devtools-mcp-XXXX/` (`TMPDIR=/workspace/artifacts`) |
| Download via the browser | `./workspace/Downloads/` (Chromium's default is `$HOME/Downloads`; `HOME=/workspace`) |

The point: `write_file` → reload `file:///workspace/...` → `take_screenshot` into the same
tree → read the PNG back with the file tools. One directory, no crossings, and it is all
yours on the host. (`artifacts/` ships empty and stays out of git; `Downloads/` appears on
first download.)

**Dev servers:** nothing execs inside this image (the MCP servers are files + browser, not a
shell). Run `npm run dev` / `vite` on the host as usual and tell the agent to test
`http://host.docker.internal:5173` — the compose file's `extra_hosts` makes that name
resolve.

### Without compose

```sh
docker run --rm -p 8090:8090 --shm-size=1g --user "$(id -u):$(id -g)" \
  -v "$PWD/data:/app/data" \
  -v "$PWD/workspace:/workspace" \
  -v "$PWD/profile:/profile" \
  -e HOME=/workspace \
  -e BARELOOP_BASE_URL=http://host.docker.internal:9931/v1 \
  -e BARELOOP_API_KEY=none \
  -e BARELOOP_MODEL=qwen3.6-27b \
  --add-host=host.docker.internal:host-gateway \
  bareloop-webdev
```

## Choosing the filesystem server

Three ways to give the agent files, in increasing order of isolation:

1. **`@modelcontextprotocol/server-filesystem`** (the baked default) — the official Node
   server, installed globally at build time; the config just names its bin. Tool names:
   `read_file`, `write_file`, `edit_file`, … Allowed roots are exactly the directories in
   `args` (`/workspace`).

2. **[mcp-vroot](https://github.com/cs8425/mcp-vroot) as a stdio binary you mount** — the
   "tools come in via `-v`" pattern. It is a single static Go binary (like bareloopd),
   sandboxed through `os.Root`, with `fs_list`/`fs_read`/`fs_view` (images and PDFs render
   multimodally) and `fs_write`/`fs_edit` under `mode: rw`:

   ```sh
   git clone https://github.com/cs8425/mcp-vroot && cd mcp-vroot
   CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o mcp-vroot .
   ```

   ```sh
   docker run --rm -p 8090:8090 --shm-size=1g --user "$(id -u):$(id -g)" \
     -v "$PWD/data:/app/data" \
     -v "$PWD/workspace:/workspace" \
     -v "$PWD/config.vroot.json:/app/config.json:ro" \
     -v "$PWD/mcp-vroot:/opt/mcp/mcp-vroot:ro" \
     -v "$PWD/vroot.json:/opt/mcp/vroot.json:ro" \
     -e BARELOOP_BASE_URL=... -e BARELOOP_API_KEY=... -e BARELOOP_MODEL=... \
     --add-host=host.docker.internal:host-gateway \
     bareloop-webdev
   ```

   Nothing new is installed in the image — the tool arrives entirely on the mount. (With
   compose: add those three mounts to `compose.yaml`'s `volumes:` — `./config.vroot.json` →
   `/app/config.json:ro`, `./mcp-vroot` → `/opt/mcp/mcp-vroot:ro`, `./vroot.json` →
   `/opt/mcp/vroot.json:ro` — the baked config is simply shadowed.)

3. **mcp-vroot over HTTP, in its own container** — when the files should not even share a
   container with the agent. Run vroot (`bind: 0.0.0.0:8765`, one of its own images or the
   repo's Dockerfile), share a network, and swap the `vroot` entry for

   ```json
   { "name": "vroot", "type": "http", "url": "http://vroot:8765/mcp" }
   ```

   Same image, different config file.

## Files

- `Dockerfile` — the image (chrome-devtools-mcp + server-filesystem baked in)
- `compose.yaml` — `docker compose up --build` and it runs (data in `./data`, project in `./workspace`, browser profile in `./profile`)
- `config.json` — baked config: chrome + filesystem, both rooted at `/workspace`
- `config.vroot.json` — variant: chrome + mcp-vroot via stdio (pattern 2 above)
- `vroot.json` — mcp-vroot's own config (workspace `/workspace`, `rw`)
- `workspace/index.html` — placeholder project, mounted at `/workspace`
- `workspace/artifacts/` — where default screenshots land (`TMPDIR`), empty in git
