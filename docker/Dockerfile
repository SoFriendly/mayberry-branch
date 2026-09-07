# syntax=docker/dockerfile:1

FROM golang:1.23-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .

# main.Version is deliberately left at its "dev" default. Both checkForUpdate and
# autoUpdateLoop in cmd/branch/main.go return immediately when Version == "dev".
# Stamping a real version here would arm the in-process self-updater, which would
# then try to rewrite /usr/local/bin/mayberry -- a root-owned binary the container
# user cannot write -- on a loop. In a container, updates come from pulling a new
# image.
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux go build -trimpath \
    -ldflags="-s -w" \
    -o /out/mayberry ./cmd/branch

FROM alpine:3.20
# /library is the scanned library root. Its contents arrive as mounts: the user's
# EPUBs read-only at /library/books, and (optionally) mirror storage at
# /library/_mirror, whose path internal/mirror/paths.go hardcodes as a child of
# the library root. Docker creates those mountpoints as root at container init,
# which is why this directory stays root-owned (mkdir's default: root:root 0755).
#
# Deliberately NOT writable by the mayberry user. If it were, then turning
# mirroring on without supplying the /library/_mirror mount would let
# EnsureMirrorRoot create the directory in the container's ephemeral upper layer
# and silently fill it with books that vanish when the container is recreated.
# Root ownership makes that fail, which the entrypoint's preflight turns into a
# clear error.
#
# /home/mayberry, by contrast, is group-writable: it seeds the mayberry-data
# volume, and a `user:` override in compose has to keep working there or the
# daemon loses branch_id/friendly_id on every restart (SaveBranch only warns).
# Group-writable + gid 1000 means `user: "<your-uid>:1000"` writes fine.
RUN apk add --no-cache ca-certificates tzdata \
    && addgroup -g 1000 mayberry \
    && adduser -D -u 1000 -G mayberry -h /home/mayberry mayberry \
    && mkdir -p /home/mayberry/.mayberry \
    && chown -R mayberry:mayberry /home/mayberry \
    && chmod -R g+w /home/mayberry \
    && mkdir -p /library

COPY --from=build /out/mayberry /usr/local/bin/mayberry
COPY --chmod=0755 docker/docker-entrypoint.sh /usr/local/bin/docker-entrypoint.sh

# uid/gid 1000 matches the first user on a typical Linux desktop, so bind-mounted
# libraries are readable out of the box. If yours differs, override the uid with
# compose's `user:` directive rather than rebuilding, but keep the gid at 1000.
USER mayberry
ENV HOME=/home/mayberry
WORKDIR /home/mayberry

EXPOSE 1950

# The daemon traps SIGINT only (signal.NotifyContext(ctx, os.Interrupt) in
# cmd/branch/main.go). Docker's default stop signal is SIGTERM, which the Go
# runtime turns into an immediate exit -- the ctx is never canceled, so the
# tunnel, watcher and mirror goroutines are killed mid-flight on every
# `docker stop` / `docker compose down` / restart. Ask for SIGINT instead so
# the existing graceful-shutdown path actually runs.
STOPSIGNAL SIGINT

# Reachability of the daemon's own status endpoint. wget is busybox's; the image
# has no curl. A plain 200 check is the whole test: needs_setup can never be true
# here, because MAYBERRY_LIBRARY is always set and needsSetup only trips on an
# empty library_path.
HEALTHCHECK --interval=30s --timeout=5s --start-period=15s --retries=3 \
    CMD wget -q -O /dev/null http://127.0.0.1:1950/api/status || exit 1

ENTRYPOINT ["docker-entrypoint.sh"]
