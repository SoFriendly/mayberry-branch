# How Mayberry Works — Technical Overview

## The Short Version

Mayberry is a federated EPUB sharing network. You keep your books on your own computer. A central catalog lets people discover them. When someone wants to download, a short-lived token authorizes the transfer through an encrypted tunnel. Nobody sees your IP. Nobody tracks what you read.

## Architecture

```
Reader (OPDS app)
    │
    ▼
mayberry.pub ─── OPDS catalog + JWT token issuer
    │
    │  signed download token
    ▼
branch.pub ──── encrypted tunnel router
    │
    │  WebSocket tunnel (outbound from your machine)
    ▼
Your computer ── serves the EPUB file
```

There are three components:

- **Town Square** (`mayberry.pub`) — The public catalog. It knows which books exist and which branch has them. It issues short-lived download tokens. It never touches the actual files.
- **Proxy Hub** (`branch.pub`) — The tunnel router. It connects readers to branches through encrypted WebSocket tunnels. It strips all identifying information from requests before forwarding them.
- **Branch** (your computer) — Your local daemon. It scans your EPUB folder, shares metadata with the catalog, and serves files to authorized requests. It connects outbound to the proxy — it never opens a port or exposes your network.

## Privacy Design

### Your IP is never exposed

Your branch connects **outbound** to `branch.pub` via a WebSocket tunnel. This is the same direction as opening a website — your router doesn't need to accept incoming connections, no ports need to be forwarded, and your IP address is never published in DNS, HTTP headers, or any public-facing response.

When someone downloads a book from your branch, the request travels:

```
Reader → branch.pub (Railway) → WebSocket tunnel → your branch
```

The reader sees `branch.pub`'s IP address (a shared cloud server hosting thousands of apps). Your branch sees the request arrive from `localhost`. The Proxy Hub actively strips all identifying headers (`X-Forwarded-For`, `CF-Connecting-IP`, `X-Real-IP`, etc.) before forwarding through the tunnel.

### No user accounts, no tracking

There are no user accounts in Mayberry. No email addresses, no passwords, no login. The system uses signed tokens for authorization, not identity.

When a reader wants to download a book:

1. Town Square issues a **short-lived JWT** (JSON Web Token) signed with an Ed25519 key. The token contains only the book's ISBN and which branch has it. It expires in 10 minutes.
2. The reader is redirected to the branch through the proxy tunnel.
3. The branch verifies the token's cryptographic signature against Town Square's public key. If valid, it serves the file.

No record is kept of who requested the token. Town Square tracks **download counts per book** (for popularity ranking) but not who downloaded what.

### What's visible, what's not

| Information | Who can see it |
|---|---|
| Your branch name (e.g., `merry-vale`) | Public — it's a subdomain |
| Which books your branch has | Public — listed in the OPDS catalog |
| Your IP address | Nobody — hidden behind outbound WebSocket tunnel |
| Who you are | Nobody — no accounts, no personal info collected |
| Who downloaded what | Nobody — no patron logs, IPs stripped at proxy |
| Download counts per book | Public — used for popularity ranking |
| Your home network | Protected — branch connects outbound, no open ports |

## Cryptography

Mayberry uses **Ed25519** for all token signing. Ed25519 is a modern elliptic curve signature scheme — the same one used by SSH, Signal, and WireGuard.

- Town Square holds the **private key** and uses it to sign download tokens and tunnel authorization tokens.
- Branches and the Proxy Hub hold the **public key** and use it to verify signatures.
- Tokens are standard **JWTs** (RFC 7519) with EdDSA signatures, inspectable and verifiable by anyone with the public key.

Token types:

| Token | TTL | Purpose |
|---|---|---|
| Download token | 10 minutes | Authorizes a single book download from a specific branch |
| Tunnel token | 24 hours | Authorizes a branch to register its WebSocket tunnel |

## Network Security

### All traffic is encrypted

- Reader ↔ Town Square: HTTPS (TLS 1.3)
- Reader ↔ Proxy Hub: HTTPS (TLS 1.3)
- Branch ↔ Proxy Hub: WSS (WebSocket over TLS 1.3)

The WebSocket tunnel between your branch and the proxy hub is always encrypted. Even if someone intercepted the tunnel traffic, they would see only TLS-encrypted data.

### The proxy strips identifying data

Before forwarding any request through the tunnel to a branch, the Proxy Hub removes:

- `X-Forwarded-For`
- `X-Real-IP`
- `CF-Connecting-IP`
- `True-Client-IP`
- `Forwarded`
- Cloudflare tracking headers (`CF-Ray`, `CF-IPCountry`, `CF-Visitor`)

The branch only sees the request method, path, and content headers needed to serve the file.

### Tunnel authentication

Branches can't just connect to the proxy and claim any subdomain. The process:

1. Branch registers with Town Square, receives a branch ID.
2. Branch requests a **tunnel token** — a JWT signed by Town Square that authorizes a specific subdomain.
3. Branch connects to the Proxy Hub and presents the tunnel token.
4. Proxy Hub verifies the token's signature and subdomain claim before accepting the tunnel.

This prevents subdomain hijacking — nobody can impersonate your branch.

## What Mayberry doesn't do

- **No DRM** — Files are served as-is. Mayberry is a library, not a storefront.
- **No user tracking** — No analytics, no cookies, no fingerprinting.
- **No central storage** — Your files stay on your machine. Town Square only has metadata (titles, authors, ISBNs).
- **No always-on requirement** — If your computer is off, your branch is simply shown as unavailable. Books from other branches are unaffected.

## Infrastructure

Town Square and the Proxy Hub run on [Railway](https://railway.app) with servers in multiple regions. The Branch daemon runs on your computer as a lightweight background service (~10 MB binary, minimal CPU and memory usage).

The OPDS catalog is compatible with any standard OPDS reader app (Cantook, KOReader, Librera, Thorium, etc.).

## Open Source

Mayberry is fully open source under the MIT license. The code is at [github.com/SoFriendly/Mayberry](https://github.com/SoFriendly/Mayberry). Every component — Town Square, Proxy Hub, and Branch — can be audited, forked, or self-hosted.
