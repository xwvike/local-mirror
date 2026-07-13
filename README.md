# local-mirror

English | [简体中文](README.zh-CN.md)

One-way directory mirroring over TCP. One machine serves a directory (the
`reality` mode), others keep a live replica of it (`mirror`), optionally
through relays chained A → B → C. A single static binary with a small custom
binary protocol — no external services, no cloud, nothing to install besides
the binary itself.

```
┌─────────────┐   tree / changes / files   ┌─────────────┐
│   reality   │ ─────────────────────────▶ │   mirror    │
│  (server)   │      custom TCP proto      │  (client)   │
└─────────────┘                            └─────────────┘
 watches the fs                             pulls, stays in sync
```

Mirroring is additive by default: files that exist only on the mirror side
are left alone. Deletion must be enabled explicitly with `--allow-delete`,
and even then the program refuses to run on critical paths (your home
directory, `/etc`, system trees) unless you also pass `--allow-critical`.

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

```
────────────────────────────────────────────────
  Local Mirror v1.0.0  ·  reality (服务器)
────────────────────────────────────────────────
  同步目录  /path/to/source
  监听地址  0.0.0.0:52345
  实例 ID   3b7b81ee
  进程 PID  62289
  日志      .local-mirror/logs/error.log (级别 error)
────────────────────────────────────────────────
```

## How it works

Both sides scan the sync directory on startup, hashing files with blake3 in
parallel. The resulting tree is persisted to bbolt (`.local-mirror/cache.db`),
so a restart only rehashes files whose size or mtime changed and prunes nodes
deleted while the process was down.

The server watches for changes with fsnotify. Directories are scored by
activity: hot ones get real-time watches, cold ones fall back to slow polling
with adaptive backoff. This keeps the watch count under the OS limit and lets
an idle laptop actually go idle. Changed directories are recorded in a
one-hour rolling journal.

The client doesn't poll. It sends a "wait for changes" request and blocks;
the server answers as soon as something changes, or after 50 seconds to keep
the connection alive. Changes typically reach the mirror within a couple of
seconds. Correctness never depends on a notification arriving: every request
queries a time interval against the journal, so a lost wakeup is simply
picked up by the next round trip. A low-frequency full rescan (every 30
minutes by default) acts as the final safety net and is forced after the
machine wakes from a long sleep.

Files are diffed by blake3 hash and size — unchanged content is never
re-transferred. Transfers are chunked, verified against the whole-file hash,
and moved into place with an atomic rename, so you never see a half-written
file. Interrupted downloads keep their partial data in `.local-mirror/partial/`
and resume from the offset; if the source file changed in the meantime the
fingerprint won't match and the download starts over. A rename within a
directory is recognized by hash and performed locally instead of
re-downloading, and mtimes are copied from the source. Relays apply changes
through the same engine and wake their own downstream immediately, so
renames stay cheap and mtimes stay accurate across every hop of a chain.

The long-poll round trip doubles as a heartbeat: TCP keepalive probes dead
peers at the OS level, the server drops connections idle for more than 90
seconds, and caps how many it accepts concurrently. If the server restarts,
the client notices the instance ID change during reconnect and rebuilds its
session with a full reconciliation.

Symlinks are not tracked at all — not synced, not dereferenced. This is
deliberate: dereferencing links would let content from outside the sync
root leak into the mirror. See [SECURITY.md](SECURITY.md).

### LAN discovery

When `-r` is omitted, mirror and relay probe the local network over UDP
multicast/broadcast (query–response, zero idle traffic). In an interactive
terminal you get a list to pick from; non-interactive environments connect
automatically when exactly one server is found. With `-k` set, probes carry
a keyed MAC and servers ignore unauthenticated scans instead of revealing
their sync paths. Discovery does not cross routers or VPN tunnels — use
`-r` there.

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
the client it means never downloaded and never deleted, even with
`--allow-delete`. Built-in defaults are only `.local-mirror`, `.git` and
`.DS_Store` — add things like `node_modules` yourself if you want them
skipped.

## Deletion safety

Syncing overwrites existing files, and `--allow-delete` removes extra ones,
so the failure mode worth designing against is pointing the tool at the
wrong directory. There are three levels:

1. **Default** — sync only, no deletion. Critical paths are refused
   outright: the home directory, filesystem roots, and system trees such as
   `/etc` or `/usr` *including their subdirectories* (`-p /etc/nginx` is
   refused just like `-p /etc`). Paths are resolved through symlinks before
   the check, so aliasing tricks don't get around it. Ordinary directories
   inside your home are not restricted.
2. **`--allow-critical`** — unlocks syncing on critical paths, still without
   deletion. The first time an existing file would be overwritten, the
   original is copied to `.local-mirror/backups/<relative path>` first.
   Later syncs never touch that backup, and if the copy fails the file is
   skipped rather than overwritten without one.
3. **`--allow-delete`** — enables deletion. On critical paths it only works
   combined with `--allow-critical`; on normal paths it is enough on its
   own.

Independently of the ladder, every path the server sends is validated to
stay inside the sync root. Anything that escapes — `..` traversal, absolute
paths — is rejected before it touches the disk, so a malicious or buggy
server cannot write or delete outside the directory you gave it.

## Encryption

Optional, via the Noise protocol (NNpsk0: X25519 + ChaCha20-Poly1305 +
BLAKE2s). Give both ends the same passphrase with `-k` and you get mutual
authentication and forward secrecy; a peer with a wrong passphrase, or one
speaking plaintext, fails the handshake immediately.

On anything but a trusted LAN — the public internet, shared Wi-Fi,
multi-tenant networks — use `-k` together with an explicit `-r`. Without
encryption the handshake is unauthenticated, and discovery replies are
plaintext with an amplification factor, so discovery is a trusted-LAN
feature only. The audit notes in [SECURITY.md](SECURITY.md) go into detail.

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

Each task runs as its own child process, so tasks share nothing and can't
take each other down. Logs are multiplexed with a `[task-name]` prefix.
Crashed tasks restart with exponential backoff (5s up to 60s); exit code 2
(missing directory, bad passphrase — configuration problems) is treated as
permanent and stops only that task. SIGTERM/SIGINT to the parent shuts down
all children, which makes the whole thing suitable as a single systemd
service.

A few practical notes: server tasks on one machine share the 52345–52354
port range (10 at most); mirror/relay tasks are better off with an explicit
`realityip` (with discovery, finding zero servers is retried with backoff —
this tolerates the upstream booting later — but finding several is treated
as a permanent configuration error); `secret` is passed to children through
the environment, so it never shows up in `ps`.

## Running under systemd

`-p` means no working-directory games are needed. A unit file is included
at [deploy/local-mirror.service](deploy/local-mirror.service):

```bash
sudo cp local-mirror /usr/local/bin/
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

- `cache.db` — the persisted directory tree; makes restarts on big trees
  fast because unchanged files aren't rehashed
- `logs/error.log` — runtime log, rotated at 10 MB keeping the last 3 files
- `partial/` — chunks of interrupted downloads awaiting resume
- `backups/` — pre-overwrite copies, only with `--allow-critical`
- `ignore` — optional ignore patterns, merged with `-i` (restart to apply)

## Building

```bash
go build -o local-mirror ./cmd/local-mirror

# cross-compile everything into dist/ (version from git describe)
./build.sh        # Linux / macOS
./build.ps1       # Windows
```

## Layout

```
cmd/local-mirror/   entry point, flag validation, banner, task supervisor
config/             flag definitions and the YAML multi-task config
internal/
  network/          protocol, server, client, port probing, LAN discovery
  tree/             directory tree, bbolt persistence, change journal
  watcher/          heat-scored watching and event filtering
  safety/           critical-path checks, path containment, overwrite backups
  logger/           logging with size-based rotation
  tui/              raw-mode picker for the discovery list
  *.go              orchestration for the three modes
pkg/                small shared helpers: hashing, paths, terminal styling
deploy/             systemd unit and YAML examples
```

Design notes for the push/long-poll architecture are in
[DESIGN.md](DESIGN.md). Known limitations and plans are in [TODO.md](TODO.md).
