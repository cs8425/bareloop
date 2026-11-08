# syntax=docker/dockerfile:1

# bareloopd as a container.
#
# The runtime image is `scratch` because the binary needs nothing else: the web UI is embedded
# (embed.go), sqlite is pure Go (wasm build, so CGO_ENABLED=0 works), the tz database is embedded
# (time/tzdata), and both paths the daemon uses by default — ./config.json and ./data — resolve
# under one working directory you get to bind-mount. The only file copied besides the binary is
# the CA bundle, for reaching LLM servers over HTTPS.
#
#   docker build -t bareloop .
#
#   docker run -d --name bareloop -p 8090:8090 --user "$(id -u):$(id -g)" \
#     -v "$PWD/data:/app/data" \
#     -v "$PWD/config.json:/app/config.json:ro" \
#     -e BARELOOP_API_KEY=... -e BARELOOP_MODEL=... \
#     bareloop
#
# The image runs unprivileged (uid 65534); the --user line hands the container your own uid
# instead — what you want when bind-mounting host directories, because files stay yours.
# Any uid works at run time without a rebuild: /app is world-writable.
#
# The config file is optional — without one the daemon starts a single "main" session — and
# ${ENV} references inside it are expanded at load, so the API key lives in the environment,
# not in the file. Tools are whatever the config says: an http MCP URL, or a binary you
# bind-mount and name with a stdio command. See samples/ for packaged variants.

FROM golang:alpine AS builder
WORKDIR /src

# go.sum's hashes are checked against the module cache; downloading deps in their own layer
# keeps `go mod download` warm across source-only rebuilds.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN --mount=type=cache,mode=0755,target=/root/.cache/go-build \
    --mount=type=cache,mode=0755,target=/go \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /bareloopd ./cmd/bareloopd

# The app directory, pre-created world-writable: scratch cannot chmod anything later, and
# whichever uid ends up running the daemon must be able to mkdir and write ./data under it.
# ./data too (mkdir -m only styles the leaf, hence two steps): an empty named volume mounted
# at /app/data inherits its mode, so a plain `docker compose up` works on the first boot.
RUN mkdir -p -m 0777 /rootfs/app && mkdir -m 0777 /rootfs/app/data

FROM scratch

# ca-certificates: the daemon is an HTTPS client (OpenAI-compatible endpoints), and scratch
# ships no trust store. (Zoneinfo is not needed — the tz database is compiled in.)
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=builder /bareloopd /bareloopd
COPY --from=builder /rootfs/app /app

# config.json and data/ are resolved relative to this directory, which is what makes the two
# -v flags above complete the deployment.
WORKDIR /app

# Run unprivileged. A bare number, because scratch has no /etc/passwd to resolve a name —
# 65534 is nobody everywhere. `docker run --user $(id -u):$(id -g)` overrides this at run
# time (no rebuild, no surprise: /app is world-writable, so any uid can create ./data).
USER 65534:65534

EXPOSE 8090

ENTRYPOINT ["/bareloopd"]
# The only override a container needs: the default bind of 127.0.0.1 is unreachable from the
# host. Note that flags beat the config file, so a mounted config's own "addr" is ignored —
# pass your own command line to replace this one.
CMD ["-addr", "0.0.0.0:8090"]
