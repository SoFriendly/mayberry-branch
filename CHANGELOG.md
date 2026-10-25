# Changelog

All notable changes to the Mayberry branch client and network.

## v0.8.31 — 2026-10-25

### Improved
- Search now matches each word of your query independently, ignoring spaces and capitalization — searching `nightvale` finds *Welcome to Night Vale*, and `vale fink` works even though the words span the title and the author. Applies to both the main catalog and each branch's own catalog.

## v0.8.30 — 2026-10-23

### Security
- Maintenance release following the October 21 Railway platform security disclosure. All network signing keys and credentials were rotated as a precaution; an audit found no misuse. This release restarts branches so they pick up the new keys. No functional changes.

## v0.8.29 — 2026-10-21

### Security
- Releases are now signed with an offline Ed25519 key. Your branch verifies every update's signature before installing it and refuses anything unsigned or tampered with — even a compromised update server can no longer push a malicious binary. Updates are also only accepted for strictly newer versions, blocking downgrade attacks.

### Added
- Each release is now tagged on GitHub with the signed binaries attached, so you can pin and audit exact versions.

### Fixed
- Replacing a book's EPUB now updates its cover everywhere: the branch re-extracts the cover, refreshes its local cache, and re-uploads it to the catalog (which previously kept the first cover it ever saw).
- Your branch's public dashboard (`your-name.branch.pub`) now shows your books to signed-in visitors — the book data was mistakenly restricted to your local network, so remote viewers saw an empty grid.
- Branches now clean up mirrored files for books that have left the catalog, reclaiming disk space automatically.

## v0.8.19 — 2026-09

### Added
- Browsing a branch's catalog now mirrors the owner's folder structure — folders appear as sections in OPDS readers instead of one flat list.

## v0.8.16 — 2026-09

### Fixed
- Mirroring no longer wastes bandwidth retrying downloads that fail deterministically (corrupt sources, hash mismatches).

## v0.8.14 — 2026-08

### Added
- Sharing controls moved to your account: manage who sees your library from the branch settings or the hosted settings page, and changes apply instantly across the network.

## Earlier highlights

- **OPDS 2.0** — every feed content-negotiates modern OPDS 2.0 JSON alongside classic Atom, with browse lanes, format facets, and proper pagination.
- **Branch-served catalogs** — each branch serves its own standalone OPDS catalog at `your-name.branch.pub/opds`, no central round-trip needed.
- **Audiobooks** — M4B audiobooks are first-class: shared, mirrored, and filterable alongside ebooks.
- **Personal catalog filters** — include or exclude branches, cap file sizes, and filter languages; preferences follow your library card everywhere.
- **Library cards** — Plex-style sharing with no passwords or emails: a nine-digit card is your identity, and sharing is mutual. Guests get cards too.
- **Mirroring** — branches volunteer spare disk to mirror rare books, so a book survives its only holder going offline.
- **Resumable downloads** — downloads survive slow uplinks and support range resume.
