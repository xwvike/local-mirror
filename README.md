# local-mirror

English | [简体中文](README.zh-CN.md)

One-way directory mirroring over TCP. One machine serves a directory (the
`reality` mode), others keep a live replica of it (`mirror`), optionally
through relays chained A → B → C. A single static binary on every platform.

```
┌─────────────┐   tree / changes / files   ┌─────────────┐
│   reality   │ ─────────────────────────▶ │   mirror    │
│  (server)   │      custom TCP proto      │  (client)   │
└─────────────┘                            └─────────────┘
 watches the fs                             pulls, stays in sync
```

Mirroring is additive by default: files that exist only on the mirror side
are left alone, deletion requires an explicit flag. Files are compared by
blake3 hash, transferred in chunks and moved into place atomically;
interrupted downloads resume. Changes normally arrive within a couple of
seconds, and a full rescan (every 30 minutes by default) catches anything a
lost notification might have missed. Symlinks are neither synced nor
dereferenced.

## Install

macOS:

```bash
brew install xwvike/tap/local-mirror
```

Windows:

```powershell
scoop bucket add xwvike https://github.com/xwvike/scoop-bucket
scoop install local-mirror
```

Debian/Ubuntu — replace `amd64` with `arm64` on ARM machines:

```bash
ver=$(curl -fsSL https://api.github.com/repos/xwvike/local-mirror/releases/latest | sed -n 's/.*"tag_name": *"v\([^"]*\)".*/\1/p')
curl -fLO "https://github.com/xwvike/local-mirror/releases/download/v${ver}/local-mirror_${ver}_linux_amd64.deb"
sudo apt install "./local-mirror_${ver}_linux_amd64.deb"
```

RHEL/Fedora — dnf takes the URL directly:

```bash
ver=$(curl -fsSL https://api.github.com/repos/xwvike/local-mirror/releases/latest | sed -n 's/.*"tag_name": *"v\([^"]*\)".*/\1/p')
sudo dnf install "https://github.com/xwvike/local-mirror/releases/download/v${ver}/local-mirror_${ver}_linux_amd64.rpm"
```

Any other Linux — the binary is static, having it on your PATH is all it
takes:

```bash
ver=$(curl -fsSL https://api.github.com/repos/xwvike/local-mirror/releases/latest | sed -n 's/.*"tag_name": *"v\([^"]*\)".*/\1/p')
curl -fL "https://github.com/xwvike/local-mirror/releases/download/v${ver}/local-mirror_${ver}_linux_amd64.tar.gz" | tar xz local-mirror
sudo install -m 755 local-mirror /usr/local/bin/
```

Or build from source:

```bash
git clone https://github.com/xwvike/local-mirror && cd local-mirror
go build -o local-mirror ./cmd/local-mirror
```

## Quick start

```bash
# serve a directory (-p defaults to the current working directory)
local-mirror -m reality -p /path/to/source

# replicate it on another machine
local-mirror -m mirror -r 192.168.1.100 -p /path/to/replica

# leave out -r to discover servers on the LAN and pick one interactively
local-mirror -m mirror -p /path/to/replica

# relay: mirror from upstream and serve downstream at the same time
local-mirror -m relay -r 192.168.1.100 -p /path/to/relay
```

On startup it prints a banner with the actual listen port, sync directory
and log location. Fair warning: the CLI output is currently in Chinese.

## Flags

| Flag | Description | Default |
|---|---|---|
| `-m, --mode` | `reality` (server), `mirror` (client), or `relay` | `reality` |
| `-p, --path` | sync root; state lives in `.local-mirror/` beneath it | working dir |
| `-r, --realityip` | upstream address for mirror/relay; empty = LAN discovery | |
| `-a, --alias` | instance name shown in discovery lists | hostname |
| `-i, --ignore` | extra ignore patterns, comma-separated | |
| `--config` | YAML config running multiple tasks (excludes the other flags) | |
| `--allow-delete` | delete local files that no longer exist upstream | off |
| `--allow-critical` | allow syncing on critical paths, with overwrite backups | off |
| `-c, --cooldown` | full-rescan interval in seconds, client side | `1800` |
| `-f, --filebuffersize` | transfer chunk size in bytes, server side | `65536` |
| `-k, --secret` | transport encryption passphrase (or env `LOCAL_MIRROR_SECRET`) | |
| `-l, --loglevel` | `debug` / `info` / `warn` / `error` | `error` |

`local-mirror --help` has the long version.

Ignore patterns (from `-i` or a `.local-mirror/ignore` file, one per line,
`#` comments) are matched per path segment at any depth and support `* ? []`
globs. On the server a match means the entry is never scanned or served; on
the client it means never downloaded and never deleted. Built-in defaults
are only `.local-mirror`, `.git` and `.DS_Store` — add things like
`node_modules` yourself if you want them skipped.

## Deletion safety

Syncing overwrites existing files, and `--allow-delete` removes extra ones,
so the failure mode worth designing against is pointing the tool at the
wrong directory. There are three levels:

1. **Default** — sync only, no deletion. Critical paths are refused
   outright: the home directory, filesystem roots, and system trees such as
   `/etc` or `/usr` including their subdirectories. Paths are resolved
   through symlinks before the check. Ordinary directories inside your home
   are not restricted.
2. **`--allow-critical`** — unlocks syncing on critical paths, still without
   deletion. Before an existing file is first overwritten, the original is
   copied to `.local-mirror/backups/<relative path>`.
3. **`--allow-delete`** — enables deletion. On critical paths it only works
   combined with `--allow-critical`; on normal paths it is enough on its
   own.

Independently of the ladder, every path the server sends is validated to
stay inside the sync root, so a malicious or buggy server cannot write or
delete outside the directory you gave it.

## Encryption

Optional, via the Noise protocol (NNpsk0). Give both ends the same
passphrase with `-k` and you get mutual authentication and forward secrecy;
a peer with a wrong passphrase, or one speaking plaintext, fails the
handshake immediately. On anything but a trusted LAN, use `-k` together
with an explicit `-r` — LAN discovery is plaintext by design and meant for
trusted networks only.

## Multiple tasks (YAML)

One machine sharing several directories, or serving one and backing up
another, can run everything from a single YAML file
(example: [deploy/local-mirror.example.yml](deploy/local-mirror.example.yml)):

```yaml
defaults:
  loglevel: info

tasks:
  - name: photos          # task name = discovery alias = log prefix
    mode: reality
    path: /srv/photos
    ignore: [cache, "*.log"]
  - name: nas-backup
    mode: mirror
    path: /srv/backup
    realityip: 192.168.1.100
    allow_delete: true
```

```bash
local-mirror --config /etc/local-mirror.yml
```

Each task runs as its own child process; crashed tasks restart with backoff,
configuration errors (exit code 2) stop only the affected task, and a
SIGTERM to the parent shuts down everything. Server tasks on one machine
share the 52345–52354 port range, so ten at most. `secret` is passed to
children through the environment and never shows up in `ps`.

## Running under systemd

A unit file example is included at
[deploy/local-mirror.service](deploy/local-mirror.service) (the deb/rpm
packages install it under `/usr/share/doc/local-mirror/examples/`):

```bash
sudo cp deploy/local-mirror.service /etc/systemd/system/
# edit mode/path/upstream in ExecStart, then:
sudo systemctl daemon-reload
sudo systemctl enable --now local-mirror
journalctl -u local-mirror -f
```

Inject the passphrase via `Environment=LOCAL_MIRROR_SECRET=...` or an
`EnvironmentFile=` rather than a `-k` argument, to keep it out of `ps`.

## Files it creates

Everything lives under `.local-mirror/` in the sync root (excluded from
syncing and from git):

- `cache.db` — the persisted directory tree; restarts skip unchanged files
- `logs/error.log` — runtime log, rotated at 10 MB keeping the last 3 files
- `partial/` — chunks of interrupted downloads awaiting resume
- `backups/` — pre-overwrite copies, only with `--allow-critical`
- `ignore` — optional ignore patterns, merged with `-i` (restart to apply)

## Development

`go build ./...` and `go test ./...` are all there is to it. Releases are
cut by pushing a `v*` tag: CI runs goreleaser, which publishes the archives,
deb/rpm packages, the Homebrew cask and the Scoop manifest in one go
(`goreleaser release --snapshot --clean` builds everything locally without
publishing).

MIT licensed.
