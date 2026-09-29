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

# Windows (Scoop; run `irm get.scoop.sh | iex` first if Scoop is not installed)
scoop bucket add xwvike https://github.com/xwvike/scoop-bucket
scoop install local-mirror

# from source
go build -o local-mirror ./cmd/local-mirror
```

## Recipes

Pairings run in the foreground. Once verified, each can be installed as a service
with the same options, as described in
[Running as a service](#running-as-a-service-recommended).

```bash
# ── 1. LAN: the source listens, the sink dials it ─────────────────────────────
# A (source). --gen-key creates a key, or reuses an existing one, and prints the
# command for B.
local-mirror --send -p /path/to/source --gen-key
# B (sink). 192.168.1.100 is the address of A.
local-mirror --receive --connect 192.168.1.100 -p /path/to/replica -k <printed-key>

# ── 2. LAN, zero config: the sink finds the source by UDP discovery ───────────
# A (source).
local-mirror --send -p /path/to/source --gen-key
# B (sink). Scans the LAN for sources and prompts for a selection.
local-mirror --receive -p /path/to/replica -k <printed-key>

# ── 3. Push over the internet: the reachable end (sink) listens ───────────────
# VPS (sink).
local-mirror --receive --listen -p /srv/backup --allow-delete --gen-key
# Client (source).
local-mirror --send --connect vps.example.net:52345 -p /path/to/source -k <printed-key>

# ── 4. Internet push, rsync-style positional form (./dir @host = push) ────────
# VPS (sink).
local-mirror --receive --listen -p /srv/backup --allow-delete --gen-key
# Client (source).
local-mirror -k <printed-key> ./path/to/source @vps.example.net:52345

# ── 5. Relay A → B → C: B pulls from A and serves C ───────────────────────────
# A (source, 192.168.1.100).
local-mirror --send -p /path/to/source --gen-key
# B (relay, 192.168.1.101).
local-mirror --send --receive --connect 192.168.1.100 -p /path/to/relay -k <printed-key>
# C (sink).
local-mirror --receive --connect 192.168.1.101 -p /path/to/replica -k <printed-key>
```

Each end stores the key in its own `.local-mirror/key` on the first run, after which
`-k` can be omitted. Running a `--gen-key` command again reuses the existing key.

## Running as a service (recommended)

local-mirror is designed to run continuously in the background and keep the replica
current; one-off copies are better served by rsync. A pairing from
[Recipes](#recipes) is first verified in the foreground, then installed as a service
with the same options: `sudo local-mirror service install <options>` on Linux, or
`local-mirror service install <options>` on macOS (a per-user service, no `sudo`).
The service starts at boot and is restarted after a failure.

Example: a Linux server receives, a Mac sends.

```bash
# ── Server: Linux, receiver, reachable as vps.example.net ─────────────────────
# Foreground run. --gen-key creates a key and prints the command for the sender.
local-mirror --receive --listen -p /srv/backup --allow-delete --gen-key
# After stopping it with Ctrl+C: install as a service. The existing key is reused.
sudo local-mirror service install --receive --listen -p /srv/backup --allow-delete --gen-key

# ── Client: macOS, sender ─────────────────────────────────────────────────────
# Foreground run.
local-mirror --send --connect vps.example.net:52345 -p ~/projects -k <printed-key>
# After stopping it with Ctrl+C: install as a service.
local-mirror service install --send --connect vps.example.net:52345 -p ~/projects -k <printed-key>
```

Service management on the Linux server. On macOS, `sudo` is omitted, the config file
is `~/.config/local-mirror/config.yml` and the log is `~/Library/Logs/local-mirror.log`.

```bash
# Config file, tasks, service state and management commands.
local-mirror service status
# Changes a directory's options by installing it again (here: deletion turned off).
sudo local-mirror service install --receive --listen -p /srv/backup --gen-key
# Applies a manually edited config. A config with errors is rejected and the
# running service is not affected.
sudoedit /etc/local-mirror/config.yml
sudo local-mirror service restart
# Removes one directory. Removing the last one stops and uninstalls the service.
sudo local-mirror service remove -p /srv/backup
# Stops and uninstalls the service. The config file is kept.
sudo local-mirror service uninstall
# Sync status and service log.
local-mirror --status --all
sudo journalctl -u local-mirror
```

Installing another directory adds a task to the same service.

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
| `--gen-key` | write a random key to `.local-mirror/key` (reuses an existing one) and print it; with run flags, start as well | |
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

- `--connect` takes a domain, IPv4, or IPv6 literal, with an optional port (`--connect [2001:db8::1]:52345`).
- `--receive` without `--connect` or `--listen` performs LAN discovery: it scans over UDP and prompts for a source (recipe 2).
- A listening sink serves one source at a time. A source connecting during an active session is rejected and retries with backoff.

## Ignore patterns

Patterns come from `-i` and/or a `.local-mirror/ignore` file (one per line, `#`
comments). Matched per path segment at any depth; `* ? []` globs supported.

- On the source, a match is never scanned or served (enumeration and direct file
  requests both refused). On the sink, never downloaded and never deleted.
- `.local-mirror` (own state dir) is always excluded and cannot be un-ignored.
- `.git` and `.DS_Store` are excluded by default; a `!` prefix includes them again,
  e.g. `local-mirror --send -p <dir> --gen-key -i '!.git'`. For `.git`, git
  push/fetch is preferable to a file-level mirror.

## What gets synced

- Regular files (content and modification time) and directories, plus the
  permission bits (`rwx`) of both. Sink directories always keep owner `rwx`.
  Permissions are not synced to or from Windows.
- Symlinks, sockets, FIFOs and device files are skipped.
- Anything the source cannot read (a file or a whole directory) is skipped, and the
  sink leaves its copy untouched, even with `--allow-delete`. Sync resumes once it
  is readable again.
- Permissions changed locally on a sink are restored on its periodic local rescan.

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
- `--gen-key` writes a random key to `.local-mirror/key` (mode 600). On a terminal
  it also prints the key and the command for the other end. An existing key file
  is reused, so the same command can be run again after a restart. Combined with
  run flags, it starts the instance in the same command. A self-chosen key should
  be a long random string (`openssl rand -base64 24`).
- Resolution order: explicit `-k` (or `secret:` in YAML) > `.local-mirror/key`
  file > plaintext. An end started with `-k` saves the key to its own key file
  (sinks, and sources that dial out), so later runs can omit `-k`.
- `local-mirror --show-key -p <dir>` prints the key (on a terminal only);
  `local-mirror --gen-key --force -p <dir>` generates a new one. Regenerating the
  key on the listening end disconnects every connected dialer.

## Observe: `--status` / `--heat`

Read-only commands for a running instance, selected by `-p` (its sync root) or by
the current directory. The daemon writes `status.json` / `heat.json` only while
one of these commands is running.

```bash
# Status of the instance whose sync root is /path/to/source.
local-mirror --status -p /path/to/source
# Status of every instance on this host.
local-mirror --status --all
# Directory heat table of a source (sinks have none).
local-mirror --heat -p /path/to/source
```

`--all` finds every instance on this host from the process table. On macOS it only
finds instances started with absolute paths.

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
hottest first and shows whether active directories are watched in real time.

## Service details

`local-mirror service install` keeps its tasks in a YAML config: `/etc/local-mirror/config.yml`
for a system service (Linux default) or `~/.config/local-mirror/config.yml` for a
per-user one (macOS default). The file can also be edited manually;
`sudo local-mirror service restart` (Linux) or `local-mirror service restart`
(macOS) applies the changes. Example:
[deploy/local-mirror.example.yml](deploy/local-mirror.example.yml).

```yaml
defaults:
  loglevel: info
  secret: <key>           # listening tasks need a key (or a .local-mirror/key in their root)
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

- Each task takes the same direction keys as the CLI (`send` / `receive` /
  `connect` / `listen`; both `send` and `receive` = relay). One task runs
  in-process; two or more each get a child process (crash → restart with backoff;
  config error = exit 2, stops only that task). Listening tasks share ports
  52345–52354 (ten max).
- The key lives in the config's `secret:` (mode 0600, readable only by the service
  user), never on a command line where `ps` would show it. **The config file must
  not live inside any task's sync root** (it would be mirrored out, secret and
  all); local-mirror refuses such a config.
- On Linux the service runs as the user who invoked `sudo`, not as root, so synced
  files are not owned by root. Another account is set with
  `sudo local-mirror service install --run-as <user> <options>`.
- The scope follows the platform default (Linux: system, macOS: per-user); every
  service command accepts `--system` or `--user` to override it.
- Init systems are detected automatically: **systemd**, **launchd**, **procd**
  (OpenWrt: `/etc/init.d/local-mirror`, output to `logread`).
- `local-mirror service install --dry-run <options>` prints the files and commands it
  would write and run, without changing the system.
- The same config also runs in the foreground: `local-mirror --config <file>`.

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
