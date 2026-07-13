#!/bin/sh
# Configure a Mayberry branch entirely from environment variables, without
# modifying the Go source.
#
# The strategy is to translate env vars into the CLI flags that cmd/branch/main.go
# already parses, then exec the unmodified binary. The binary's own LoadBranch /
# SaveBranch cycle takes care of merging these into ~/.mayberry/branch.json and
# preserving branch_id / friendly_id across restarts, so this script never touches
# that file. It also means mirror_size, mirror_rate and mirror_serve_rate are
# validated by main()'s existing checks rather than by a reimplementation here.
set -eu

: "${MAYBERRY_NAME:?MAYBERRY_NAME is required (your branch's public display name)}"

# -library, -server and -hub read these env vars natively as their flag defaults
# (see envOr in cmd/branch/main.go), so they need no flag plumbing here.
export MAYBERRY_LIBRARY="${MAYBERRY_LIBRARY:-/library}"

# audiobook_path is deliberately unsupported in this image. It is the one setting
# with no CLI flag: upstream only exposes it via POST /api/setup, and calling that
# against a running daemon re-fires the setup callback, which rebuilds the HTTP
# server and restarts the tunnel/watcher/heartbeat goroutines a second time.
# Supporting it would mean merging audiobook_path into ~/.mayberry/branch.json
# with jq here, before exec, so LoadBranch picks it up on startup like any other
# field. That is where such a merge would go -- right here, ahead of the exec.

# The mirror writes into <library>/_mirror, a path hardcoded by
# internal/mirror/paths.go, which is why compose mounts mirror storage at exactly
# that path rather than somewhere of its own choosing. Without that mount
# EnsureMirrorRoot fails and internal/mirror/manager.go merely logs
# "mirror: disabling" and returns, leaving the daemon running with mirroring
# silently dead. Catch it here instead, while we can still say why.
case "${MAYBERRY_MIRROR_NETWORK:-false}" in
  true | True | TRUE | 1 | t | T)
    probe="$MAYBERRY_LIBRARY/_mirror/.write-test"
    if ! touch "$probe" 2>/dev/null; then
      echo "mayberry: MAYBERRY_MIRROR_NETWORK is on, but $MAYBERRY_LIBRARY/_mirror is not writable." >&2
      echo "mayberry: uncomment the mirror volume in docker-compose.yaml and set MAYBERRY_MIRROR_PATH." >&2
      exit 1
    fi
    rm -f "$probe"
    ;;
esac

# Mirror flags are passed unconditionally, with defaults matching config.go's.
# Omitting a flag makes main() fall back to whatever the persisted branch.json
# holds from a previous run (it gates on flag.Visit), which would let a value
# deleted from compose silently survive in the named volume. Always passing them
# keeps the container's configuration declarative.
set -- --daemon
set -- "$@" -name "$MAYBERRY_NAME"
set -- "$@" "-mirror-network=${MAYBERRY_MIRROR_NETWORK:-false}"
set -- "$@" -mirror-size "${MAYBERRY_MIRROR_SIZE:-100G}"
set -- "$@" -mirror-rate "${MAYBERRY_MIRROR_RATE:-slow}"
set -- "$@" -mirror-serve-rate "${MAYBERRY_MIRROR_SERVE_RATE:-200K}"
set -- "$@" -mirror-only "${MAYBERRY_MIRROR_ONLY:-}"
set -- "$@" -mirror-ignore "${MAYBERRY_MIRROR_IGNORE:-}"

# -port is left at its 1950 default. main() assigns cfg.Port = *port
# unconditionally, so a custom value in branch.json would be stomped back to 1950
# on every restart anyway. Host-side flexibility comes from the compose port map.
exec mayberry "$@"
