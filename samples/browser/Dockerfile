# syntax=docker/dockerfile:1

# A complete browser agent in one image: bareloopd with chrome-devtools-mcp as an MCP server
# and a Chromium for it to drive. Give it a data directory and a port and it is ready — the
# config is baked in, so there is nothing else to mount.
#
# Build the base image first (this one copies the binary out of it):
#
#   docker build -t bareloop ../..                          # from this directory
#   docker build -t bareloop-browser .                      # here (context = this folder)
#
# Run it (see README.md for the environment variables and the smoke prompt):
#
#   docker run --rm -p 8090:8090 --shm-size=1g --user "$(id -u):$(id -g)" \
#     -v "$PWD/data-browser:/app/data" \
#     -e BARELOOP_BASE_URL=http://host.docker.internal:8901/v1 \
#     -e BARELOOP_API_KEY=none -e BARELOOP_MODEL=your-model \
#     --add-host=host.docker.internal:host-gateway \
#     bareloop-browser

FROM bareloop:latest AS bareloop

FROM node:26-trixie-slim

# Debian's chromium rather than Google Chrome: same package the desktop image in _tmp uses
# (fonts and GPU-ish libraries included, CJK included — headless renders pages, and a page
# with Chinese text in it turns to tofu without the fonts). chrome-devtools-mcp officially
# supports only Chrome/Chrome for Testing; chromium works in practice, see README.md.
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

# Global installs, so the config can name the binaries directly and a running container
# never has to reach npm. Pin the versions when you want reproducible rebuilds:
# the MCP server bin is chrome-devtools-mcp (the package also ships a `chrome-devtools` CLI).
ARG CHROME_DEVTOOLS_MCP_VERSION=latest
RUN npm install -g "chrome-devtools-mcp@${CHROME_DEVTOOLS_MCP_VERSION}" \
    && npm cache clean --force

# The silence flags are belt and braces: the baked config already passes
# --no-usage-statistics, and these env vars also cover the update check.
ENV CHROME_DEVTOOLS_MCP_NO_USAGE_STATISTICS=1 \
    CHROME_DEVTOOLS_MCP_NO_UPDATE_CHECKS=1 \
    HOME=/tmp

COPY --from=bareloop /bareloopd /bareloopd
# Relative to the context = this directory; a .dockerignore here keeps the build context
# to just the config (the run-time directories next door never enter it).
COPY config.json /app/config.json

# World-writable, so the daemon can mkdir ./data under whichever uid runs it — the image's
# own 65534, or yours via --user. /artifacts is the screenshot/download root the baked
# config names (--filesystemRoot); it exists in-image so a bare `docker run` can save even
# with nothing mounted, and a bind-mount simply shadows it. (HOME=/tmp above: the
# unprivileged uid has no home, and Chromium's throwaway profile wants a writable one.)
RUN mkdir -p /artifacts && chmod 0777 /app /artifacts

USER 65534:65534

WORKDIR /app
EXPOSE 8090
ENTRYPOINT ["/bareloopd"]
CMD ["-addr", "0.0.0.0:8090"]
