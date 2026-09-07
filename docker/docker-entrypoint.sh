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

# Normalize MAYBERRY_MIRROR_NETWORK to a plain true/false up front. The set of
# spellings Go's flag package accepts (strconv.ParseBool) is narrower than what
# people reach for: "yes" would sail past a naive truthiness test here and then
# make flag.Parse die with a usage dump. Decide once, reject the rest by name.
case "${MAYBERRY_MIRROR_NETWORK:-false}" in
  true | True | TRUE | 1 | t | T | yes | Yes | YES | on | On | ON)
    mirror_network=true ;;
  false | False | FALSE | 0 | f | F | no | No | NO | off | Off | OFF | "")
    mirror_network=false ;;
  *)
    echo "mayberry: MAYBERRY_MIRROR_NETWORK must be true or false, got '${MAYBERRY_MIRROR_NETWORK}'." >&2
    exit 1
    ;;
esac

# The mirror writes into <library>/_mirror, a path hardcoded by
# internal/mirror/paths.go, which is why compose mounts mirror storage at exactly
# that path rather than somewhere of its own choosing. Without that mount
# EnsureMirrorRoot fails and internal/mirror/manager.go merely logs
# "mirror: disabling" and returns, leaving the daemon running with mirroring
# silently dead. Catch it here instead, while we can still say why.
if [ "$mirror_network" = true ]; then
  probe="$MAYBERRY_LIBRARY/_mirror/.write-test"
  if ! touch "$probe" 2>/dev/null; then
    echo "mayberry: MAYBERRY_MIRROR_NETWORK is on, but $MAYBERRY_LIBRARY/_mirror is not writable by uid $(id -u)." >&2
    echo "mayberry: two things have to be true --" >&2
    echo "mayberry:   1. the /library/_mirror volume in docker-compose.yaml is uncommented, and" >&2
    echo "mayberry:   2. the host directory it points at (MAYBERRY_MIRROR_PATH) is writable by uid $(id -u)." >&2
    echo "mayberry: Docker creates a missing host path as root, so a first run with the default" >&2
    echo "mayberry: ./data/mirror needs: mkdir -p ./data/mirror && chown $(id -u):$(id -g) ./data/mirror" >&2
    exit 1
  fi
  rm -f "$probe"
fi

# Mirror flags are passed unconditionally, with defaults matching config.go's.
# Omitting a flag makes main() fall back to whatever the persisted branch.json
# holds from a previous run (it gates on flag.Visit), which would let a value
# deleted from compose silently survive in the named volume. Always passing them
# keeps the container's configuration declarative.
set -- --daemon
set -- "$@" -name "$MAYBERRY_NAME"
set -- "$@" "-mirror-network=$mirror_network"
set -- "$@" -mirror-size "${MAYBERRY_MIRROR_SIZE:-100G}"
set -- "$@" -mirror-rate "${MAYBERRY_MIRROR_RATE:-slow}"
set -- "$@" -mirror-serve-rate "${MAYBERRY_MIRROR_SERVE_RATE:-200K}"
set -- "$@" -mirror-only "${MAYBERRY_MIRROR_ONLY:-}"
set -- "$@" -mirror-ignore "${MAYBERRY_MIRROR_IGNORE:-}"

# -port is left at its 1950 default. main() assigns cfg.Port = *port
# unconditionally, so a custom value in branch.json would be stomped back to 1950
# on every restart anyway. Host-side flexibility comes from the compose port map.
exec mayberry "$@"
