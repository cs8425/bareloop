# syntax=docker/dockerfile:1

# A web-development agent: bareloopd + chrome-devtools-mcp + a filesystem MCP server, all in
# one container, working on a bind-mounted /workspace. The agent writes the page with the
# file tools and tests it in the browser it drives itself — file:///workspace/index.html is
# the same page from both sides.
#
#   docker build -t bareloop ../..
#   docker build -t bareloop-webdev .
#
#   docker run --rm -p 8090:8090 --shm-size=1g --user "$(id -u):$(id -g)" \
#     -v "$PWD/data-webdev:/app/data" \
#     -v "$PWD/site:/workspace" \
#     -e BARELOOP_BASE_URL=http://host.docker.internal:8901/v1 \
#     -e BARELOOP_API_KEY=none -e BARELOOP_MODEL=your-model \
#     --add-host=host.docker.internal:host-gateway \
#     bareloop-webdev

FROM bareloop:latest AS bareloop

FROM node:26-trixie-slim

RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        chromium \
        ca-certificates \
        libgbm1 \
        libegl1 \
        libgl1-mesa-dri \
        fonts-noto-cjk \
        fonts-noto-color-emoji \
    && rm -rf /var/lib/apt/lists/*

# Both MCP servers as global installs — the config names their bins directly.
ARG CHROME_DEVTOOLS_MCP_VERSION=latest
ARG SERVER_FILESYSTEM_VERSION=latest
RUN npm install -g \
      "chrome-devtools-mcp@${CHROME_DEVTOOLS_MCP_VERSION}" \
      "@modelcontextprotocol/server-filesystem@${SERVER_FILESYSTEM_VERSION}" \
    && npm cache clean --force

ENV CHROME_DEVTOOLS_MCP_NO_USAGE_STATISTICS=1 \
    CHROME_DEVTOOLS_MCP_NO_UPDATE_CHECKS=1 \
    HOME=/tmp

COPY --from=bareloop /bareloopd /bareloopd
# Relative to the context = this directory; a .dockerignore here keeps the build context
# to just the config (the run-time directories next door never enter it).
COPY config.json /app/config.json

# The project directory. Bind-mount your site over it; the filesystem MCP server is
# sandboxed to exactly this path. Both directories are world-writable so whichever uid runs
# (the image's 65534, or yours via --user) can create and edit files before any mount lands.
RUN mkdir -p /workspace/artifacts && chmod 0777 /app /workspace /workspace/artifacts

USER 65534:65534

WORKDIR /app
EXPOSE 8090
ENTRYPOINT ["/bareloopd"]
CMD ["-addr", "0.0.0.0:8090"]
