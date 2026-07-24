# local-mirror

<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="assets/wordmark-dark.svg">
    <img src="assets/wordmark.svg" width="480" alt="LOCAL-MIRROR">
  </picture>
</p>

English | [简体中文](README.zh-CN.md)

One-way directory mirroring over TCP. One end is the **source** (`--send`),
the other keeps a live replica as the **sink** (`--receive`), optionally
through relays chained A → B → C.

```
┌─────────────┐   tree / changes / files   ┌─────────────┐
│    source   │ ─────────────────────────▶ │     sink    │
│   --send    │             TCP            │  --receive  │
└─────────────┘                            └─────────────┘
 watches the fs                             pulls, stays in sync
```

Data direction and transport direction are independent: **either end can be
the one that dials or the one that listens**.

The sink is additive by default: deletion requires an explicit flag at
startup. Files are compared by blake3 hash, transferred in chunks and moved
into place atomically; interrupted downloads resume. Changes normally arrive
within a couple of seconds, and a full rescan (every 30 minutes by default)
catches anything a lost notification might have missed. Symlinks are neither
synced nor dereferenced.

## Install

macOS:

```bash
brew install xwvike/tap/local-mirror
```

Windows, via [Scoop](https://scoop.sh) (no Scoop yet? `irm get.scoop.sh | iex`
installs it):

```powershell
scoop bucket add xwvike https://github.com/xwvike/scoop-bucket
scoop install local-mirror
```

Linux (any distro; also works on macOS without Homebrew):

```bash
curl -fsSL https://raw.githubusercontent.com/xwvike/local-mirror/main/install.sh | sh
```

Prebuilt deb/rpm packages are on the
[releases page](https://github.com/xwvike/local-mirror/releases).

Or build from source:

```bash
git clone https://github.com/xwvike/local-mirror && cd local-mirror
go build -o local-mirror ./cmd/local-mirror
```

## Quick start

```bash
# serve a directory (-p defaults to the current working directory)
local-mirror --send -p /path/to/source

# replicate it on another machine
local-mirror --receive --connect 192.168.1.100 -p /path/to/replica

# leave out --connect to discover sources on the LAN and pick one interactively
local-mirror --receive -p /path/to/replica

# relay: receive from upstream and serve downstream at the same time
local-mirror --send --receive --connect 192.168.1.100 -p /path/to/relay
```

Reverse who listens to push across the public internet: a server with a
public IP is the sink, the source dials out to it:

```bash
# machine A (public IP)
local-mirror --receive --listen -p /srv/backup --allow-delete
# machine B (no public IP)
local-mirror --send --connect a.example.net:52345 -p /path/to/source

# same thing, rsync-style positional sugar (./dir @host = push)
local-mirror ./path/to/source @vps.example.net:52345
```

On startup it prints a banner with the actual listen port, sync directory
and log location:

```
█   █▀█ █▀▀ █▀█ █     █▄ ▄█ ▀█▀ █▀█ █▀█ █▀█ █▀█
█   █ █ █   █▀█ █  ▀▀ █ ▀ █  █  █▀▄ █▀▄ █ █ █▀▄
▀▀▀ ▀▀▀ ▀▀▀ ▀ ▀ ▀▀▀   ▀   ▀ ▀▀▀ ▀ ▀ ▀ ▀ ▀▀▀ ▀ ▀

────────────────────────────────────────────────
  Local Mirror 2.0.0  ·  reality (server)
────────────────────────────────────────────────
  Sync root  /path/to/source
  Ignores    .local-mirror, .git, .DS_Store
  Listen     :52345 (IPv4 + IPv6)
  Encryption on (Noise NNpsk0, fp 3f9a…c71e)
  Instance   3b7b81ee
  PID        62289
  Log        .local-mirror/logs/error.log (level warn)
────────────────────────────────────────────────
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
| `--config` | YAML config running multiple tasks (excludes the other flags) | |
| `--allow-delete` | delete extra files on the sink that no longer exist upstream | off |
| `--allow-critical` | allow syncing on critical paths, with overwrite backups | off |
| `-k, --secret` | transport encryption key (or env `LOCAL_MIRROR_SECRET`) | |
| `--gen-key` | generate a random key into `.local-mirror/key`, print it, exit | |
| `--show-key` | print the existing key file and exit | |
| `--no-encrypt` | force plaintext even when a key file exists | |
| `--status` | print a running instance's status and exit (`--all` for every one) | |
| `--heat` | print a running source's directory heat table and exit | |
| `-c, --cooldown` | full-rescan interval in seconds, sink side | `1800` |
| `-f, --filebuffersize` | transfer chunk size in bytes, source side | `65536` |
| `-l, --loglevel` | `debug` / `info` / `warn` / `error` | `error` |

`local-mirror --help` has the long version.

### Direction and transport

Two independent axes: **direction** (`--send` / `--receive`) and **transport**
(`--connect` / `--listen`). Combine them freely: `--receive --listen` on a
reachable server, `--send --connect` from behind NAT. `--connect` takes a
domain name, an IPv4, or an IPv6 literal (`--connect [2001:db8::1]:52345`);
listeners bind both IPv4 and IPv6. Domain names are re-resolved on every
reconnect, so DDNS just works. Give both `--send` and `--receive` to relay.

### LAN discovery

A `--receive` with neither `--connect` nor `--listen` scans the local network
for sources over UDP and, if several answer, lets you pick one interactively.
It is the zero-config path for two machines on the same LAN — no address to
type, no port to remember. It only kicks in in that exact case: give
`--connect` and you dial a known host directly; give `--listen` and you wait
to be dialed. Discovery stays on the local segment and does not cross VPNs,
subnets or firewalls — reach across those with `--connect <host>` instead. If
a key is set, probes and replies are authenticated so a scanner without the
key learns nothing.

Ignore patterns (from `-i` or a `.local-mirror/ignore` file, one per line,
`#` comments) are matched per path segment at any depth and support `* ? []`
globs. On the server a match means the entry is never scanned or served; on
the client it means never downloaded and never deleted. `.local-mirror` (the
tool's own state dir) is always excluded and cannot be un-ignored; `.git` and
`.DS_Store` are excluded by default but removable — prefix a pattern with `!`
to sync it (e.g. `-i '!.git'`). Note `.git` is a live database: replicate
repositories with git itself (push/fetch), not a file-level mirror. Add things
like `node_modules` yourself if you want them skipped.

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

## Encryption

Optional, via the Noise protocol (NNpsk0). Give both ends the same passphrase
with `-k` for mutual authentication and forward secrecy; a wrong passphrase,
or a peer speaking plaintext, fails the handshake. On anything but a trusted
LAN, set `-k` explicitly.

Use a long random string for `-k` (e.g. `openssl rand -base64 24`).

To skip inventing one, let the tool generate it. `--gen-key` writes a strong
random key to `.local-mirror/key` (mode 600), prints it once and exits; add
run flags to generate and start in the same command. A key file is loaded
automatically when `-k` is omitted, so the listening end generates it and the
dialing end passes it in with `-k` just once (the dialer then saves its own
copy):

```bash
# on the listening end
local-mirror --gen-key --send            # prints the key, then serves
# on the dialing end, first connection only
local-mirror --receive --connect vps.example.net -k <generated-key>
```

Resolution order is explicit `-k` (including the env var) > `.local-mirror/key`
file > plaintext. `--show-key` prints the existing file, `--no-encrypt` forces
plaintext even when one is present, and `--gen-key --force` regenerates. Don't
delete the key file on the listening side while dialers are connected —
regenerating it disconnects every one of them.

## Watching a running instance

`--status` is a separate, read-only command. It works on demand: while a
`--status` is watching, the daemon writes its state to
`.local-mirror/status.json` once a second and the command renders it; when no
one is watching, the daemon writes nothing, so observing is free to leave off.
Point it at a sync root with `-p` (or run it from inside one).

```bash
local-mirror --status -p /path/to/source
```

```
──────────────────────────────────────────────────────
  Status      ● running   pid 62289 · up 3h12m
──────────────────────────────────────────────────────
  Direction   send · source   (listen)
  Peer        inbound
  Link        ● serving 192.168.1.50:54012
  Encryption  on (Noise NNpsk0)
  Sync root   /path/to/source
──────────────────────────────────────────────────────
  Transfer    ▶ docs/report.pdf
              ██████████░░░░░░░░░░  4.2 MB / 8.1 MB   5.3 MB/s
  Totals      1.2 GB / 3841 files   · last 2s ago (docs/report.pdf)
  Errors      0
──────────────────────────────────────────────────────
  CPU         1.4%
  Memory      31 MB rss   (8.1 MB heap · 22 MB sys)
  FDs         14
  Goroutines  27
──────────────────────────────────────────────────────
```

Add `--all` to find running `local-mirror` processes from the process table
and print one row each (`--config` deployments get the same table keyed by
task name):

```bash
local-mirror --status --all
```

```
  NAME             DIR    LINK  RATE        FILES   LAST      CPU    MEM
  proj/src         send   ●     5.3 MB/s    3841    2s        1.4%   31 MB
  srv/backup       recv   ●     —           912     1m        0.2%   18 MB
  media/photos     send   ○     —           220     14m       0.0%   12 MB
```

## Multiple tasks (YAML)

One machine sharing several directories, or serving one and backing up
another, can run everything from a single YAML file
(example: [deploy/local-mirror.example.yml](deploy/local-mirror.example.yml)):

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

Each task takes the same `--send` / `--receive` / `--connect` / `--listen`
direction as the command line: `send: true` serves, `receive:` replicates,
`connect:` dials a host, `listen: true` waits to be dialed, both `send` and
`receive` relay.

```bash
local-mirror --config /etc/local-mirror.yml
```

Each task runs as its own child process; crashed tasks restart with backoff,
configuration errors (exit code 2) stop only the affected task, and a
SIGTERM to the parent shuts down everything. Server tasks on one machine
share the 52345–52354 port range, so ten at most. `secret` is passed to
children through the environment and never shows up in `ps`.

## Running as a service

Linux, with systemd — a unit file example is included at
[deploy/local-mirror.service](deploy/local-mirror.service) (the deb/rpm
packages install it under `/usr/share/doc/local-mirror/examples/`):

```bash
sudo cp deploy/local-mirror.service /etc/systemd/system/
# edit the direction/path/upstream in ExecStart, then:
sudo systemctl daemon-reload
sudo systemctl enable --now local-mirror
journalctl -u local-mirror -f
```

macOS, with launchd — `brew services` only manages formulas, not casks, so
use the LaunchAgent example at
[deploy/com.xwvike.local-mirror.plist](deploy/com.xwvike.local-mirror.plist)
(also shipped inside the release archives). It starts at login and restarts
the process if it dies:

```bash
cp deploy/com.xwvike.local-mirror.plist ~/Library/LaunchAgents/
# edit the binary path, direction/path/upstream and log path, then:
launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/com.xwvike.local-mirror.plist
# stop and unload:
launchctl bootout gui/$(id -u)/com.xwvike.local-mirror
```

Either way, inject the passphrase through the environment
(`Environment=`/`EnvironmentFile=` in the unit, `EnvironmentVariables` in
the plist) rather than a `-k` argument, to keep it out of `ps`.

## Peeking at the watch tiers

A source scores every directory by activity and watches the hot ones in real
time while polling the cold ones lazily; events raise a directory's score and
idleness decays it. Read the table with `--heat` (a separate, read-only
command, like `--status`); the source publishes it to `.local-mirror/heat.json`
only while you're watching:

```bash
local-mirror --heat -p /path/to/source   # or --heat --all for every source
```

```
  heat   /path/to/source
  tier1 (real-time watch) 3/512 · tier2 (lazy poll) 40 · 43 dirs
  SCORE     TIER   EVENTS   DIRECTORY
  128.45    tier1  3410     assets/img
   42.10    tier1  890      src
    3.20    tier2  12       docs
```

Directories are listed hottest first with their score, tier and event count —
handy for checking whether the directories you actually work in got real-time
watches. A sink builds no such table.

## Files it creates

Everything lives under `.local-mirror/` in the sync root (excluded from
syncing and from git):

- `cache.db` — the persisted directory tree; restarts skip unchanged files
- `key` — self-managed transport key (mode 600), auto-loaded when `-k` is
  omitted; never synced (`--gen-key` writes it, `--show-key` prints it)
- `status.json` — live runtime status, written only while `--status` watches; discardable
- `heat.json` — directory heat table, written only while `--heat` watches (source side); discardable
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
