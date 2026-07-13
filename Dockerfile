# syntax=docker/dockerfile:1

FROM golang:1.23-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .

# main.Version is deliberately left at its "dev" default. Both checkForUpdate and
# autoUpdateLoop in cmd/branch/main.go return immediately when Version == "dev".
# Stamping a real version here would arm the in-process self-updater, which would
# then try to rewrite /usr/local/bin/mayberry -- a root-owned binary the container
# user cannot write -- on a loop. In a container, updates come from pulling a new
# image.
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath \
    -ldflags="-s -w" \
    -o /out/mayberry ./cmd/branch

FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata \
    && addgroup -g 1000 mayberry \
    && adduser -D -u 1000 -G mayberry -h /home/mayberry mayberry \
    && mkdir -p /home/mayberry/.mayberry \
    && chown -R mayberry:mayberry /home/mayberry \
    && mkdir -p /library

# /library is the scanned library root. Its contents arrive as mounts: the user's
# EPUBs read-only at /library/books, and (optionally) mirror storage at
# /library/_mirror, whose path internal/mirror/paths.go hardcodes as a child of
# the library root. Docker creates those mountpoints as root at container init,
# which is why this directory can stay root-owned.
#
# Deliberately NOT writable by the mayberry user. If it were, then turning
# mirroring on without supplying the /library/_mirror mount would let
# EnsureMirrorRoot create the directory in the container's ephemeral upper layer
# and silently fill it with books that vanish when the container is recreated.
# Root ownership makes that fail, which the entrypoint's preflight turns into a
# clear error.
RUN chown root:root /library && chmod 755 /library

COPY --from=build /out/mayberry /usr/local/bin/mayberry
COPY docker-entrypoint.sh /usr/local/bin/docker-entrypoint.sh
RUN chmod +x /usr/local/bin/docker-entrypoint.sh

# uid/gid 1000 matches the first user on a typical Linux desktop, so bind-mounted
# libraries are readable out of the box. If yours differs, override with compose's
# `user:` directive rather than rebuilding.
USER mayberry
ENV HOME=/home/mayberry
WORKDIR /home/mayberry

EXPOSE 1950

# Reachability of the daemon's own status endpoint. wget is busybox's; the image
# has no curl. A plain 200 check is the whole test: needs_setup can never be true
# here, because MAYBERRY_LIBRARY is always set and needsSetup only trips on an
# empty library_path.
HEALTHCHECK --interval=30s --timeout=5s --start-period=15s --retries=3 \
    CMD wget -q -O /dev/null http://127.0.0.1:1950/api/status || exit 1

ENTRYPOINT ["docker-entrypoint.sh"]
