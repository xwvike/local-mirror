# local-mirror

<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="assets/wordmark-dark.svg">
    <img src="assets/wordmark.svg" width="480" alt="LOCAL-MIRROR">
  </picture>
</p>

English | [简体中文](README.zh-CN.md)

One-way directory mirror over TCP. One end is the **source** (`--send`), the
other keeps a live replica as the **sink** (`--receive`). Give one process both
flags to relay (A → B → C).

```
┌─────────────┐   tree / changes / files   ┌─────────────┐
│    source   │ ─────────────────────────▶ │     sink    │
│   --send    │             TCP            │  --receive  │
└─────────────┘                            └─────────────┘
 watches the fs                             pulls, stays in sync
```

## Install

```bash
# Linux (any distro)
curl -fsSL https://raw.githubusercontent.com/xwvike/local-mirror/main/install.sh | sh

# macOS
brew install xwvike/tap/local-mirror

# Windows (Scoop; `irm get.scoop.sh | iex` first if you don't have Scoop)
scoop bucket add xwvike https://github.com/xwvike/scoop-bucket
scoop install local-mirror

# from source
go build -o local-mirror ./cmd/local-mirror
```

## Recipes

```bash
# serve a directory (-p defaults to the current working directory)
local-mirror --send -p /path/to/source

# replicate it from another machine, dialing a known host
local-mirror --receive --connect 192.168.1.100 -p /path/to/replica

# replicate over the LAN with zero config: no --connect/--listen → scan for
# sources and pick one interactively
local-mirror --receive -p /path/to/replica

# relay: pull from upstream and serve downstream in one process
local-mirror --send --receive --connect 192.168.1.100 -p /path/to/relay

# push across the public internet: the reachable end is the sink and listens,
# the source dials out. A listening end MUST set a key (see Encryption).
local-mirror --receive --listen -p /srv/backup --allow-delete --gen-key   # prints a key
local-mirror --send --connect a.example.net:52345 -p /path/to/source -k <printed-key>

# same push, rsync-style positional form (./dir @host = push)
local-mirror ./path/to/source @a.example.net:52345 -k <printed-key>
```

## Flags

| Flag | Description | Default |
|---|---|---|
| `--send` | this end is the source: data flows out | |
| `--receive` | this end is the sink: data flows in (both = relay) | |
| `--connect` | dial the peer at `host[:port]`; the peer must be listening | |
| `--listen` | wait for the peer to dial in | |
| `-p, --path` | sync root; state lives in `.local-mirror/` beneath it | working dir |
| `-a, --alias` | instance name shown in discovery lists | hostname |
| `-i, --ignore` | extra ignore patterns, comma-separated | |
| `--config` | YAML config file (mutually exclusive with the other flags) | |
| `--allow-delete` | delete sink files that no longer exist upstream | off |
| `--allow-critical` | allow syncing on critical paths, with overwrite backups | off |
| `-k, --secret` | transport encryption key (or `secret:` in the YAML config) | |
| `--gen-key` | write a random key to `.local-mirror/key`, print it, exit | |
| `--show-key` | print the existing key file and exit | |
| `--no-encrypt` | force plaintext even when a key file exists | |
| `--status` | print a running instance's status and exit (`--all` for every one) | |
| `--heat` | print a running source's directory heat table and exit | |
| `-c, --cooldown` | full-rescan interval in seconds, sink side | `1800` |
| `-f, --filebuffersize` | transfer chunk size in bytes, source side | `65536` |
| `-l, --loglevel` | `debug` / `info` / `warn` / `error` | `error` |


## Direction & transport

Two independent axes: **direction** (`--send` / `--receive`) and **transport**
(`--connect` / `--listen`). Combine freely.

- `--connect` takes a domain, IPv4, or IPv6 literal (`local-mirror --connect [2001:db8::1]:52345`);
- `--receive` → LAN discovery: scan over UDP, pick a source interactively (`local-mirror --receive`).

## Ignore patterns

Patterns come from `-i` and/or a `.local-mirror/ignore` file (one per line, `#`
comments). Matched per path segment at any depth; `* ? []` globs supported.

- On the source, a match is never scanned or served (enumeration and direct file
  requests both refused). On the sink, never downloaded and never deleted.
- `.local-mirror` (own state dir) is always excluded and cannot be un-ignored.
- `.git` and `.DS_Store` are excluded by default but removable — prefix `!` to
  sync (e.g. `-i '!.git'`). For `.git`, prefer git push/fetch over a file-level
  mirror.

## Deletion safety

Syncing overwrites existing files; `--allow-delete` removes extra ones. Three
levels, by sync root:

| Root | no flag | `--allow-critical` | `--allow-critical` + `--allow-delete` | `--allow-delete` only |
|---|---|---|---|---|
| normal | sync, no delete | same | sync + delete | sync + delete |
| **critical** | **refused** | sync + overwrite backup | sync + delete + overwrite backup | **refused** |

Critical = home directory, filesystem roots, system trees (`/etc`, `/usr`, …)
including subdirectories; resolved through symlinks first. Backups land in
`.local-mirror/backups/<relative path>` before the first overwrite.

## Encryption

Noise protocol (NNpsk0). Same `-k` passphrase on both ends → mutual auth and
forward secrecy; a wrong key or a plaintext peer fails the handshake.

- **A listener binds all interfaces, so a plaintext listener refuses to start.**
  Any non-loopback listener must set a key, or pass `--no-encrypt` to insist on
  plaintext (trusted LAN only). Dialers are unaffected.
- `--gen-key` writes a strong random key to `.local-mirror/key` (mode 600),
  prints it once, and (with run flags) starts in the same command. Use a long
  random string if you supply your own (`openssl rand -base64 24`).
- Resolution order: explicit `-k` (or `secret:` in YAML) > `.local-mirror/key`
  file > plaintext. The dialing end saves its own copy after the first connect.
- `--show-key` prints the file; `--gen-key --force` regenerates. Regenerating on
  the listening end disconnects every connected dialer.

## Observe: `--status` / `--heat`

Read-only, on-demand commands run against a live instance (point `-p` at its sync
root, or run from inside one). The daemon only writes `status.json` / `heat.json`
while one of these is watching.

```bash
local-mirror --status -p /path/to/source     # add --all for every process
local-mirror --heat   -p /path/to/source     # source only; sinks have no heat table
```

```
──────────────────────────────────────────────────────
  Status      ● running   pid 62289 · up 3h12m
  Direction   send · source   (listen)
  Link        ● serving 192.168.1.50:54012
  Encryption  on (Noise NNpsk0)
  Transfer    ▶ docs/report.pdf  ██████████░░░░░░  4.2 MB / 8.1 MB  5.3 MB/s
  Totals      1.2 GB / 3841 files    Errors 0
──────────────────────────────────────────────────────
```

`--heat`: a source scores each directory by activity, watches hot ones in real
time (tier1) and polls cold ones lazily (tier2). The table lists directories
hottest first — useful to confirm active directories got real-time watches.

## Multiple tasks (YAML)

Run several directories from one file (example:
[deploy/local-mirror.example.yml](deploy/local-mirror.example.yml)):

```yaml
defaults:
  loglevel: info
tasks:
  - name: photos          # task name = discovery alias = log prefix
    send: true
    path: /srv/photos
    ignore: [cache, "*.log"]
  - name: nas-backup
    receive: true
    connect: 192.168.1.100
    path: /srv/backup
    allow_delete: true
```

```bash
local-mirror --config /etc/local-mirror.yml
```

Each task takes the same direction keys as the CLI (`send` / `receive` /
`connect` / `listen`; both `send` and `receive` = relay). One task runs in-process;
two or more each get a child process (crash → restart with backoff; config error
= exit 2, stops only that task; SIGTERM to the parent stops all). Server tasks
share ports 52345–52354 (ten max). `secret` reaches children over stdin — not in
`ps`, not in the environment. **The config file must not live inside any task's
sync root** (it would be mirrored out, secret and all); local-mirror refuses such
a config.

## Run as a service

```bash
local-mirror service install     # --system on Linux, --user on macOS by default
# prints the config path; fill in tasks, then:
sudo systemctl enable --now local-mirror                    # Linux (systemd)
launchctl kickstart -k gui/$(id -u)/com.xwvike.local-mirror # macOS (launchd)

local-mirror service status      # where config + service live, and registration state
local-mirror service uninstall   # deregister + remove service file; config kept
local-mirror service install --dry-run   # print what it would write/run, touch nothing
```

- Config path: `/etc/local-mirror/config.yml` (system) or
  `~/.config/local-mirror/config.yml` (user).
- Init systems auto-detected: **systemd**, **launchd**, **procd** (OpenWrt →
  `/etc/init.d/local-mirror`, output to `logread`).
- On Linux the service runs as the invoking user (not root, so synced files
  aren't owned by root); `--run-as <user>` picks another. Keep the passphrase in
  the config's `secret:` (mode 0600) or a `.local-mirror/key` file, never in
  `-k` (visible in `ps`).

## Files under `.local-mirror/`

In the sync root, excluded from syncing and from git:

| File | Purpose |
|---|---|
| `cache.db` | persisted directory tree; restarts skip unchanged files |
| `key` | self-managed transport key (mode 600), auto-loaded when `-k` omitted |
| `status.json` | live status, written only while `--status` watches; discardable |
| `heat.json` | heat table, written only while `--heat` watches (source); discardable |
| `logs/error.log` | runtime log, rotated at 10 MB, keeps last 3 |
| `partial/` | chunks of interrupted downloads awaiting resume |
| `backups/` | pre-overwrite copies, only with `--allow-critical` |
| `ignore` | optional ignore patterns, merged with `-i` (restart to apply) |

## Development

```bash
go build ./...
go test ./...
```

Releases are cut by pushing a `v*` tag: CI runs goreleaser, publishing the
archives, the Homebrew cask and the Scoop manifest in one go
(`goreleaser release --snapshot --clean` builds locally without publishing).

MIT licensed.
