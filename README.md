# Mayberry

**Share your EPUB library with the world.**

Mayberry is a lightweight daemon that shares your EPUB collection through a federated library network. You keep your books on your own computer. A central catalog lets people discover them. Downloads happen through encrypted tunnels — nobody sees your IP.

> EPUB format only. No DRM. No tracking. No user accounts.

## Install

**macOS & Linux:**
```sh
curl -fsSL https://mayberry.pub/install.sh | sh
```

**Windows (PowerShell):**
```powershell
irm https://mayberry.pub/install.ps1 | iex
```

**From source:**
```sh
go build -o mayberry ./cmd/branch
```

## Usage

```sh
mayberry
```

On first run, open `http://localhost:1950` in your browser to:
1. Choose a **branch name** (your public URL becomes `your-name.branch.pub`)
2. Select your **EPUB library folder** (scanned recursively)

Mayberry installs as a background service and runs automatically on login.

### Commands

| Command | Description |
|---------|-------------|
| `mayberry` | Start (setup wizard on first run) |
| `mayberry update` | Update to the latest version |
| `mayberry version` | Show installed version |
| `mayberry service uninstall` | Stop and remove the background service |
| `mayberry service deregister` | Remove your branch from the network |

### Settings

Open `http://localhost:1950/settings` anytime to change your branch name or library folder.

## How It Works

Your branch connects **outbound** to the Mayberry network through an encrypted WebSocket tunnel. No ports are opened on your machine. Your IP is never exposed.

When someone downloads a book:
1. The catalog issues a short-lived signed token (Ed25519 JWT, 10 min TTL)
2. The request is routed through the encrypted tunnel to your machine
3. Your branch verifies the token and serves the EPUB

See [SECURITY.md](SECURITY.md) for the full technical details.

## What Gets Extracted

Mayberry reads metadata from your EPUB files:
- Title and author (from OPF)
- ISBN (from OPF identifiers or copyright pages)
- Publication date (from `dc:date`)
- Genres (from `dc:subject` tags)
- Cover image (EPUB2 and EPUB3 patterns)

No file contents are uploaded — only metadata is shared with the catalog.

## Supported Formats

| Format | Supported |
|--------|-----------|
| `.epub` | Yes (EPUB 2 and 3) |
| `.pdf` | No |
| `.mobi` | No |

## Platform Support

| OS | Architecture | Install |
|----|-------------|---------|
| macOS | Intel & Apple Silicon | `curl` or Homebrew |
| Linux | x86_64 & ARM64 | `curl` |
| Windows | x86_64 | PowerShell |

## Configuration

Config is stored at `~/.mayberry/branch.json`.

| Flag | Env Var | Default | Description |
|------|---------|---------|-------------|
| `-library` | `MAYBERRY_LIBRARY` | — | EPUB library folder |
| `-name` | — | auto-generated | Branch display name |
| `-server` | `MAYBERRY_SERVER` | `https://mayberry.pub` | Catalog server |
| `-hub` | `MAYBERRY_HUB` | `https://branch.pub` | Tunnel server |
| `-port` | — | `1950` | Local dashboard port |
| `--daemon` | — | `false` | Run without TUI |

## License

MIT
