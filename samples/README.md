# samples

Packaged agents built on the image this repo's `Dockerfile` produces. Each sample is one
`docker build` + one `docker run` — or just `docker compose up` inside the sample directory:
the config (including its MCP tools) is baked in, the directories it writes are committed,
and the environment falls back to sensible defaults, so first run needs zero flags.

| Sample | Tools baked in | What it can do |
|---|---|---|
| [`browser/`](browser/README.md) | chrome-devtools-mcp + Chromium | drive and inspect a real browser |
| [`webdev/`](webdev/README.md) | chrome-devtools-mcp + Chromium + filesystem MCP | write a site under `/workspace`, open it, fix it |

No Docker? [`windows/`](windows/README.md) is the same idea for Windows boxes: two static
binaries plus a config — one layout where the file tools and the browser share a folder,
one where the file tools sit in an IIS site folder while the browser is sandboxed (and
enforced to stay) outside it.

The plain image (the root `Dockerfile`, no sample) carries no tools on purpose: give it
tools through the mounted config instead — an `"http"` MCP URL, or a
`"stdio"` command naming a binary you `-v` in (the webdev sample documents that pattern
with a mounted [mcp-vroot](https://github.com/cs8425/mcp-vroot)).

What every sample's run command needs (the sample `compose.yaml` files all do this for you):

- a base image: the samples copy the binary out of `bareloop:latest`, so
  `docker build -t bareloop .` (repo root) is the one-time prerequisite; compose files
  build the sample image itself (`--build`).
- `-p 8090:8090` — the unauthenticated API + UI port; keep it loopback-bound
  (`-p 127.0.0.1:8090:8090`) if something else on the machine can see your interface.
- `-v ...:/app/data` — sessions live here; omit it and they die with the
  container.
- `--user "$(id -u):$(id -g)"` — the images already run unprivileged (uid 65534), but pass
  your own uid when bind-mounting host directories and files written by the agent stay
  yours. No rebuild is needed to switch: every directory a container writes (`/app`,
  `/workspace`) is world-writable, so any uid works. The compose files default to uid 1000
  (the first regular Linux account — the owner of a fresh clone); if yours differs, put
  `BARELOOP_UID=$(id -u)` and `BARELOOP_GID=$(id -g)` in a `.env` next to the compose file.
- The repo root also carries a `compose.yaml` for the plain image (named volume for data,
  no config until you mount one): `docker compose up --build` from the root, then open
  http://localhost:8090.
- `-e BARELOOP_BASE_URL / BARELOOP_API_KEY / BARELOOP_MODEL` — expanded into the config at
  startup; the LLM always lives outside the container.
- On Linux, `--add-host=host.docker.internal:host-gateway` if the LLM is on the host.
