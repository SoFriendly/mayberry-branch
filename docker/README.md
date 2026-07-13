# Docker

Run Mayberry declaratively as a container, fully configured through environment variables. No setup wizard, no interactive prompts.

## Layout

The image is built from `docker/Dockerfile`, but `docker-compose.yaml` and your `.env` live at the repo root, because that is where Docker's conventions want them: the build context has to be the repo root (that is where the Go source is), and compose only discovers a compose file in the current directory or its parents.

Run every `docker compose` command below **from the repo root**, not from this directory.

## Quick Start

```sh
cp docker/.env.sample .env
# Edit .env and set MAYBERRY_NAME and MAYBERRY_LIBRARY_PATH
docker compose up -d
```

The container starts, registers your branch with the catalog, and begins serving your library. View the dashboard at `http://localhost:MAYBERRY_PORT/` (default 1950).

## Configuration

Every Mayberry setting (except audiobook paths) can be set via environment variables and `.env` file:

| Variable | Required | Default | Description |
|----------|----------|---------|-------------|
| `MAYBERRY_NAME` | Yes | — | Your branch's public display name; sanitized to `https://<name>.branch.pub` |
| `MAYBERRY_LIBRARY_PATH` | Yes | — | Host path to your EPUB folder; mounted read-only at `/library/books` inside the container |
| `MAYBERRY_PORT` | No | `1950` | Host-side port for the dashboard; container always listens on 1950 internally |
| `MAYBERRY_SERVER` | No | `https://mayberry.pub` | Catalog server URL (only change for custom servers) |
| `MAYBERRY_HUB` | No | `https://branch.pub` | Tunnel hub URL (only change for custom tunnels) |
| `MAYBERRY_MIRROR_NETWORK` | No | `false` | Enable network mirror (opt-in failover mirroring of other branches) |
| `MAYBERRY_MIRROR_SIZE` | No | `100G` | Max disk space for mirrored books; use K/M/G/T suffixes (only if mirroring on) |
| `MAYBERRY_MIRROR_ONLY` | No | — | Comma-separated whitelist of branch subdomains to mirror; empty = mirror all (only if mirroring on) |
| `MAYBERRY_MIRROR_IGNORE` | No | — | Comma-separated blocklist of branch subdomains to never mirror; empty = block none (only if mirroring on) |
| `MAYBERRY_MIRROR_RATE` | No | `slow` | Mirror download speed: `slow` / `normal` / `fast` (only if mirroring on) |
| `MAYBERRY_MIRROR_SERVE_RATE` | No | `200K` | Outbound bandwidth cap when serving mirror requests, e.g. `200K`, `5M` (only if mirroring on) |
| `MAYBERRY_MIRROR_PATH` | No | `./data/mirror` | Host path where mirrored books land; only applies if the `/library/_mirror` volume is uncommented in `docker-compose.yaml` |

See `docker/.env.sample` for documentation of each variable.

## Key Differences from Native Install

**Library mount is read-only.** By design, Mayberry never writes to your library folder. The container has read permission only, so your books cannot be accidentally modified or deleted.

**Mirroring needs its own volume.** Network mirroring (`MAYBERRY_MIRROR_NETWORK=true`) downloads other branches' books into `<library>/_mirror`. Since the library itself is read-only, the mirror lives on a separate bind mount (commented out in `docker-compose.yaml`). If you enable mirroring without uncommenting that mount, the container will fail at startup with a clear error.

**Audiobook libraries are not supported.** `audiobook_path` is the one Mayberry setting with no CLI flag; the Docker entrypoint has no way to set it without writing `~/.mayberry/branch.json` itself (which the design deliberately avoids). Audiobook support via Docker can be added later if upstream exposes a flag.

**Environment variables are authoritative.** Every mirror flag is re-applied on container restart, whether from `.env` file or explicit `docker compose up` flags. The web dashboard's setup wizard is not the source of truth for Docker deployments; editing `.env` and running `docker compose up -d` is how you change settings.

**Branch identity persists.** Your `branch_id` and `friendly_id` (and cached cover images) live in the `mayberry-data` named volume and survive container restarts and image upgrades. They are never lost unless you explicitly delete the volume.

**Healthcheck is observability only.** The image includes a `HEALTHCHECK` that probes `/api/status` every 30 seconds. `docker compose ps` shows whether the daemon is healthy. However, Docker Compose does not automatically restart unhealthy containers—that requires an external supervisor like [docker-autoheal](https://github.com/willfarrell/docker-autoheal).

## Stopping and Cleanup

Stop the container:
```sh
docker compose down
```

Delete the container and remove the named volume (loses branch identity and cached covers):
```sh
docker compose down -v
```

Remove the built image:
```sh
docker rmi mayberry-branch:latest
```

## Troubleshooting

**Container exits immediately:**
- Check that `MAYBERRY_NAME` is set: `docker compose config` should fail with `:?` error if unset.
- Check logs: `docker compose logs mayberry`.

**Dashboard unreachable at `localhost:PORT`:**
- Confirm the port mapping: `docker compose ps` should show `0.0.0.0:PORT->1950/tcp`.
- If MAYBERRY_PORT is unset, it defaults to 1950. Verify your `.env` file.

**Mirroring enabled but silently disabled:**
- Check logs: `docker compose logs mayberry | grep mirror`.
- If the log says "mirror: disabling", the `/library/_mirror` mount is likely missing or not writable. Uncomment the mirror volume in `docker-compose.yaml` and run `docker compose up -d` again.

**Branch identity changed after container recreation:**
- This should not happen—the `mayberry-data` named volume should persist. Check: `docker volume ls | grep mayberry` should show a volume named `mayberry-branch_mayberry-data`.
- If the volume was deleted, identity is lost. Re-register your branch by starting fresh.

**Permission errors on your EPUB folder:**
- The container runs as uid/gid 1000 by default. If your host library is owned by a different uid, uncomment the `user:` line in `docker-compose.yaml` and set it to your uid/gid, e.g. `user: "1001:1001"`.
- The library mount is read-only anyway, so permission to *read* is all that matters. Writes are not allowed.

## Docker-Specific Environment Variables

The following environment variables are specific to the Docker entrypoint and are only used when running via `docker-compose.yaml`:

- `MAYBERRY_NAME` — translated to the `-name` CLI flag
- `MAYBERRY_MIRROR_NETWORK`, `MAYBERRY_MIRROR_SIZE`, `MAYBERRY_MIRROR_ONLY`, `MAYBERRY_MIRROR_IGNORE`, `MAYBERRY_MIRROR_RATE`, `MAYBERRY_MIRROR_SERVE_RATE` — translated to their corresponding `-mirror-*` CLI flags

The following environment variables are read natively by `cmd/branch/main.go` (with or without Docker) and don't need Docker's translation layer:

- `MAYBERRY_LIBRARY` — read by the `-library` flag's default
- `MAYBERRY_SERVER` — read by the `-server` flag's default
- `MAYBERRY_HUB` — read by the `-hub` flag's default

See `docker/docker-entrypoint.sh` for implementation details.
