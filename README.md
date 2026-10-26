# sbox

Runs AI agent skills inside a Firecracker microVM with fake secrets and a
fake internet, and reports what they try to do. Work in progress: see
[MILESTONE.md](MILESTONE.md) for what's done and what's next.

```
macOS ── limactl ──▶ Lima VM "sbox" (vz, nested virtualization, no host mounts)
                     └─ Firecracker ──▶ microVM per run (node, python, uv, strace)
```

## Requirements

- Apple Silicon M3 or later, macOS 15+ (nested virtualization)
- Lima 2.x: `brew install lima`
- Go 1.27.2+ to build

## Install

```sh
go install github.com/incu6us/sbox@latest   # or, from a clone: go build -o sbox .
sbox setup
```

## Commands

```
sbox setup
sbox doctor
sbox run [-timeout <duration>] [-v] <dir> -- <command> [args...]
sbox scan [-timeout <duration>] [-v] <dir>
```

### `sbox setup`

Creates, provisions and checks everything; safe to run again at any time.

1. Creates the Lima VM `sbox` if it doesn't exist (vz, nested virtualization,
   4 CPUs, 6 GiB RAM, 30 GiB disk, no host folders mounted), or starts it if
   it's stopped.
2. Provisions it as root (`scripts/provision.sh`): packages, the `sbox-vm`
   user the jailer runs Firecracker as, and into `/opt/sbox` Firecracker,
   the jailer, the guest kernel and the guest image (`rootfs.ext4`, ~830 MB).
3. Installs `/opt/sbox/bin/sbox-run`, which runs one microVM per call.
4. Runs `sbox doctor`.

Every download is checked against a SHA-256 pinned in `setup.go` (Firecracker
v1.17.0, guest kernel 6.1.155, Node v24.21.0, uv 0.13.0); a mismatch aborts
and caches nothing. The guest's Ubuntu packages come from the signed archive
via mmdebstrap. Downloads are cached; the image is rebuilt only when
`ROOTFS_REVISION` in `scripts/provision.sh` or a pin changes. The first run
takes about a minute.

Exit codes: `0` everything works, `1` setup or doctor failed, `2` bad usage.

### `sbox doctor`

Checks the whole chain in about 7 seconds and prints one line per check:

| Check | Passes when |
|---|---|
| `limactl`, `vm`, `nested virtualization` | Lima is installed, the `sbox` VM is running with nested virtualization |
| `kvm` | `/dev/kvm` exists in the VM |
| `firecracker`, checksums, `rootfs`, `runner` | the pinned versions are installed, `sbox-run` and its user exist |
| `microVM boot`, `boot time`, `guest node/uv/python/strace` | a microVM boots and reports the expected toolchain |
| `run: unprivileged guest user` | a real `sbox run` executes the command as uid 1000 |
| `run: read-only root` | the guest can't modify the shared image |
| `run: guest reaches VM side` | the guest reaches 172.16.0.1 on its tap |
| `run: cut off from internet` | the guest can't reach the internet |
| `run: DNS lookups logged` | a lookup in the guest reaches the DNS log |

Each failure says what to do (usually `sbox setup`). Exit codes: `0` all
checks pass, `1` a check failed, `2` bad usage.

### `sbox run`

```sh
./sbox run ./some-skill -- node --version
./sbox run -timeout 30s ./some-skill -- sh -c 'ls; python3 x.py'
./sbox run -v ./some-skill -- npm test          # also kernel and Firecracker output
```

Runs `<command>` on a copy of `<dir>` in a fresh microVM, in about 1.4 s plus
the command's own time. The `--` is required: everything after it is the
command, passed through exactly (quotes, spaces and `$` arrive intact).

| Flag | Meaning |
|---|---|
| `-timeout <duration>` | Stop the microVM after this long (Go duration: `30s`, `5m`). Default `1m`, minimum `1s`. |
| `-v` | Also show kernel and Firecracker output, marked `·`. Useful when a run fails to start. |

Guest output is shown live, marked `│`, then a summary line on stderr
(`sbox: exit 3 in 1.4s`).

| Exit code | Meaning |
|---|---|
| the command's | it ran: its own status |
| `124` | timed out |
| `125` | sbox failed: bad usage, VM not set up (`sbox setup`), or the microVM stopped before the command started (`-v` shows why) |

What each run gets:

- a fresh microVM, started by Firecracker's jailer as the unprivileged
  `sbox-vm` user in its own chroot, cgroup and network namespace
- a network namespace holding only the run's tap device: the guest reaches
  the VM side (172.16.0.1) and nothing else, with no route out
- a read-only root filesystem (the shared image), with scratch (`/tmp`,
  `/home/runner`) in tmpfs that disappears with the VM
- `<dir>` copied to `/home/runner/work` (up to 256 MiB, symlinks kept as
  links); the command runs there as `runner` (uid 1000)
- output with control characters escaped, so the guest can't drive your
  terminal
- result markers carrying a per-run nonce the command can't read, so it can't
  fake its exit status; a timeout is reported outside the guest's console
- cleanup of the run's jail, drive, cgroup and namespace afterwards, also on
  timeout

### `sbox scan`

```sh
./sbox scan ~/.claude/plugins/cache/<marketplace>/<plugin>/<version>
./sbox scan -timeout 30s -v ./some-skill      # longer per entry point, show their output
```

Finds the skill's automatic entry points and runs each one in a single
fresh microVM, as `runner`, under strace:

| Entry point | Found in | Run as |
|---|---|---|
| hook commands | `hooks.json`, `settings.json`, `plugin.json` (Claude Code and Cursor layouts) | the command, with a realistic event JSON on stdin and `CLAUDE_PLUGIN_ROOT` / `CLAUDE_PROJECT_DIR` set |
| stdio MCP servers | `.mcp.json`, `mcp.json`, `plugin.json` | the server, sent `initialize` and `tools/list`, kept running until the timeout |
| npm install scripts | root `package.json` (`preinstall`, `install`, `postinstall`, `prepare`) | `npm run-script <hook>` |
| `setup.py` | root | `python3 setup.py egg_info` |

Dependency folders are skipped. For each entry point the report shows its exit
status and what it did: processes started, network connections attempted
(with destination), and files read, written, deleted or renamed outside system
paths. Example, for a plugin's SessionStart hook:

```
1/2  hook SessionStart (hooks/hooks.json)
     $ command -v node >/dev/null 2>&1 && node "${CLAUDE_PLUGIN_ROOT}/hooks/activate.js" || exit 0
     exit 0
     processes  node /home/runner/work/hooks/activate.js
     network    none
     files      read /home/runner/.config/plugin/config.json (ENOENT)
                write /home/runner/.claude/.plugin-active
```

| Flag | Meaning |
|---|---|
| `-timeout <duration>` | Stop each entry point after this long. Default `20s`. |
| `-v` | Also show the entry points' own output. |

How it stays trustworthy: strace runs as root and writes each trace to a
root-only directory, so an entry point can't edit the record of what it did.
Every `runner` process is killed before the next entry point starts. The
network is blocked as for `sbox run`, so connections show up as attempts, and
hostname lookups show the names, attributed to the entry point that made them
(see [DNS](#dns)). Exit codes: `0` the scan completed, `125` sbox failed.
Verdicts come in milestone 6.

### DNS

Each run's namespace has a DNS logger on 172.16.0.1, and the guest's
`resolv.conf` points at it. It answers every lookup with NXDOMAIN and records
the name and record type. It never forwards and never returns an address, so
a lookup fails exactly as it would without it and nothing leaves the
machine. `sbox run` lists the names in its summary; `sbox scan` shows them per
entry point:

```
     network    93.184.215.14:80 → attempted
                DNS example.com (A, AAAA) → NXDOMAIN
```

The log lives on the VM side, outside the guest, and is sent after the
microVM exits, marked with the run's nonce, so the guest can't forge it.
`sbox scan` looks up a sentinel name containing the nonce before each entry
point, to attribute every lookup to the entry point that made it. The logger
runs as the unprivileged `sbox-vm` user, and is killed with everything else
in the namespace when the run ends.

## Managing the VM

```sh
limactl stop sbox       # free the 4 CPUs and 6 GiB; sbox setup starts it again
limactl delete sbox     # remove it completely; sbox setup recreates it
limactl shell sbox      # a shell inside the VM (files are in /opt/sbox)
```

## Development

```sh
go test ./...
golangci-lint run ./...
```

VM-side scripts live in `scripts/` and are embedded in the binary:
`provision.sh` (setup), `doctor.sh` (doctor's VM checks), `sbox-run.sh` (one
microVM), `guest-init.sh` (runs inside each microVM), `sbox-dns.py` (the DNS
logger; `python3 scripts/sbox-dns.py --selftest`).
