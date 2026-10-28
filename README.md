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
- 10 GiB free disk
- Go 1.27.2+ to build
- Lima: `sbox setup` installs its own pinned copy (2.2.1) when none is on `PATH`

## Install

```sh
go install github.com/incu6us/sbox@latest   # or, from a clone: go build -o sbox .
sbox setup
```

## Commands

```
sbox setup
sbox doctor
sbox run [-timeout <duration>] [-v] [-egress <host:port,...>] <dir> -- <command> [args...]
sbox scan [-timeout <duration>] [-v] [-json] [-no-prefetch] [-prefetch-timeout <duration>] <dir>

sbox dev [-fresh] <dir> [-- <command> [args...]]
sbox start <dir> -- <command> [args...]
sbox attach <session-id> [-- <command> [args...]]
sbox sync [-apply] <dir>
sbox ps
sbox stop|pause|resume|log <session-id>
sbox clean
```

### `sbox setup`

Creates, provisions and checks everything; safe to run again at any time.

1. Checks the Mac: an Apple M3 or later, macOS 15 or later, 10 GiB free.
   If no `limactl` is found, downloads Lima 2.2.1, checks its SHA-256, and
   installs it in `~/.local/share/sbox/lima` (no Homebrew, no sudo).
2. Creates the Lima VM `sbox` if it doesn't exist (vz, nested virtualization,
   4 CPUs, 6 GiB RAM, 30 GiB disk, no host folders mounted), or starts it if
   it's stopped.
3. Provisions it as root (`scripts/provision.sh`): packages, the `sbox-vm`
   user the jailer runs Firecracker as, and into `/opt/sbox` Firecracker,
   the jailer, the guest kernel and the guest image (`rootfs.ext4`, ~830 MB).
4. Installs `/opt/sbox/bin/sbox-run`, which runs one microVM per call.
5. Runs `sbox doctor`.

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
| `session: guest runs`, `pause freezes the guest`, `resume continues it`, `stop leaves nothing` | a session whose guest counts once a second keeps counting, stops counting while paused, continues after resume, and is gone from `sbox ps` after stop |

Each failure says what to do (usually `sbox setup`). Exit codes: `0` all
checks pass, `1` a check failed, `2` bad usage.

### Sessions

`sbox dev <dir>` opens a shell in a microVM with the directory copied
in, at `/home/runner/work`. Pass a command to run that instead:
`sbox dev ./myapp -- claude`. When you exit, sbox asks whether to keep the
session running (the default); a kept or paused session is what the next
`sbox dev` on that folder attaches to, resuming it if paused. Without a
terminal to ask on, the session stops at exit. The terminal is
real (colours, Ctrl-C, full-screen programs); it runs over SSH inside the
VM, with a key made per session and the guest's host key pinned.

The runner's home is kept per project between `sbox dev` sessions: the
copy of the project and what was done to it, the agent's own state,
caches, anything installed in the home. The project is copied in only the
first time, so work done inside is never overwritten. `sbox dev -fresh
<dir>` stops the project's session and starts over from the Mac's copy; `sbox clean` drops
every project's state. Only one session runs per project at a time.

Ports and services come from the config file: `sbox.yaml` in the
project's folder, and `~/.config/sbox/config.yaml` for every project
(lists from both are combined; a key sbox doesn't know is an error):

```yaml
ports: [3000, 5173]   # the guest's dev servers, on the Mac's localhost
services: [5432]      # the Mac's Postgres, reached in the guest as host.sbox:5432
egress:               # hosts the guest may reach; nothing else is reachable
  - api.anthropic.com # port 443 when left out
  - "*.github.com"    # any name under github.com (not github.com itself)
  - github.com:22     # raw TCP: git over SSH
```

Egress entries on port 443 (or `:*`) go through an HTTPS proxy that
tunnels to the listed hosts only; the guest's `HTTPS_PROXY` points at it,
in the command and in every terminal. An exact host on another port, like
`github.com:22`, is raw TCP: the guest's lookup of that name is answered
with the VM side of its network, where a relay carries the connection to
that host alone, so one host per such port.

Toolchains beyond Node, Python, uv and Claude Code go into the guest
image from the Ubuntu archive, set in `~/.config/sbox/config.yaml` only
(the image is shared by every project) and applied by `sbox setup`, which
rebuilds the image when the list changes:

```yaml
image:
  packages: [golang-go, rustc]
```

The guest's root is read-only; anything installed into the home
(`~/.local`, rustup, `uv python install`) lands on the project's kept
drive and is there next session. The runner can have 2048 processes and
threads, so a fork bomb or a runaway build fails inside the guest.

Nothing else crosses: the guest's network still has no route out, and
each forwarded port is a pair of relays joined by a unix socket in the
Lima VM. Ports must be 1024 or higher, and free in the VM (a second
session forwarding the same port is refused).

`sbox sync <dir>` shows what changed in the guest and on the Mac since
they last agreed, with diffs; `sbox sync -apply <dir>` brings each side's
changes to the other. A file changed differently on both sides is listed
as a conflict and left alone. Never synced: symlinks, `.git` (each side
keeps its own history: commit the synced files on the Mac),
`node_modules`, `.venv`, `__pycache__`, `.DS_Store`.

```
$ sbox sync ./myapp
from the guest:
  changed  notes.md
  added    util.js
from the Mac:
  changed  app.js
--- mac/notes.md
+++ guest/notes.md
@@ -1,2 +1,2 @@
 line one
-line two
+line 2, edited
dry run: sbox sync -apply ./myapp does it
```

`sbox start <dir> -- <command>` runs the same job as `sbox run`, but the
microVM keeps running after the command returns. It prints the session id
once the guest is up; `sbox attach <id>` opens a terminal in it.

```sh
id=$(sbox start ./myapp -- npm run dev)
sbox ps                 # ID, STATE (running, paused, exited), AGE
sbox pause "$id"        # freeze the microVM in memory
sbox resume "$id"
sbox log "$id"          # what the guest printed on its console
sbox stop "$id"         # end it and remove what it left in the VM
```

A session ends when its command exits or on `sbox stop`. One started more
than a day ago is stopped by the next `sbox start`. Pause and resume go
through Firecracker's API; the session's memory stays allocated while
paused.

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
| `-egress <host:port,...>` | Let the command reach these hosts, and nothing else, through a CONNECT proxy outside the guest (`HTTPS_PROXY` is set for it). The summary lists allowed and refused connections. Without it, the guest has no way out. |

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
| hook commands | `hooks.json`, `settings.json`, `plugin.json` (Claude Code and Cursor layouts) | the command, with a realistic event JSON on stdin (tool hooks get a call to the tool their matcher names, e.g. `Edit` for `Edit\|Write`) and `CLAUDE_PLUGIN_ROOT` / `CLAUDE_PROJECT_DIR` set |
| stdio MCP servers | `.mcp.json`, `mcp.json`, `plugin.json` | the server, sent `initialize` and `tools/list`, kept running until the timeout |
| npm install scripts | root `package.json` (`preinstall`, `install`, `postinstall`, `prepare`) | `npm run-script <hook>` |
| `setup.py` | root | `python3 setup.py egg_info` |

When the skill has npm dependencies (and doesn't bundle `node_modules`), a
`requirements*.txt`, or MCP servers launched with `npx`/`uvx`, the scan first
downloads them (see [Dependencies](#dependencies)), and adds an
`npm rebuild` entry point that runs the dependencies' install scripts.

Dependency folders are skipped. For each entry point the report shows its exit
status, what it did (processes started, network connections attempted with
their destination, hostnames looked up, files read, written, deleted or
renamed outside system paths) and the findings, then a verdict:

```
1/1  hook SessionStart (hooks/hooks.json)
     $ curl -s https://example.com …
     exit 0
     processes  curl -s -m 3 https://example.com
     network    93.184.215.14:80 → attempted
                DNS example.com (A, AAAA) → NXDOMAIN
     files      read /home/runner/.curlrc (ENOENT)
     findings   HIGH     tries to connect to 93.184.215.14:80
                MEDIUM   looks up example.com
                MEDIUM   runs curl

verdict: REVIEW  (0 critical, 1 high, 2 medium, 0 low)
```

#### Findings

| Rule | Severity | When an entry point… |
|---|---|---|
| `canary-exfiltration` | CRITICAL | sends a planted fake credential out: in a DNS lookup, or as an argument to a network tool |
| `possible-exfiltration` | CRITICAL | reads credentials **and** tries the network |
| `canary-argv` | HIGH | puts a planted fake credential on another program's command line |
| `credential-read` | HIGH | reads `~/.ssh`, `~/.aws`, gcloud/azure/kube/docker/gh config, `.netrc`, `.npmrc`, `.pypirc`, `.git-credentials`, `.gnupg`, shell history or `~/.env`, even if missing |
| `connect` | HIGH | tries to connect to a non-local address |
| `agent-config-write` | HIGH | writes agent configuration: `settings.json`, `CLAUDE.md`, `AGENTS.md`, `.mcp.json`, skills, commands, rules, hooks |
| `persistence` | HIGH | writes shell startup files, `authorized_keys`, autostart, systemd or cron |
| `temp-exec` | HIGH | runs a program from `/tmp`, `/var/tmp`, `/dev/shm` or `~/.cache` |
| `dns-lookup` | MEDIUM | looks up a hostname |
| `network-tool` | MEDIUM | runs curl, wget, nc, socat, ssh, scp, rsync… |
| `escalation` | MEDIUM | runs sudo, su, doas or pkexec |
| `environ-read` | MEDIUM | reads a process environment through `/proc` |
| `agent-dir-write` | LOW | writes anything else under `~/.claude`, `~/.cursor`, `~/.codex` |
| `timeout` | LOW | is still running at the timeout |

Verdict, as skills-audit: any critical → **BLOCK**, medium or high →
**REVIEW**, otherwise **PASS**. `-json` prints the same report as one object
(`target`, `verdict`, `counts`, and per entry point `kind`, `name`, `source`,
`command`, `ran`, `exit_code`, `processes`, `network`, `files`, `dns`,
`findings`).

| Flag | Meaning |
|---|---|
| `-timeout <duration>` | Stop each entry point after this long. Default `20s`. |
| `-v` | Also show the entry points' own output. |
| `-json` | Print the report as JSON. |
| `-no-prefetch` | Don't download dependencies first. |
| `-prefetch-timeout <duration>` | Stop the dependency download after this long. Default `3m`. |

How it stays trustworthy: strace runs as root and writes each process's trace
to a root-only directory, so an entry point can't edit the record of what it
did. The host rebuilds the process tree from the traces, so each event is
attributed to the program that caused it; sbox's own helpers (npm in the
`npm rebuild` entry point) aren't judged, only what they launch.
Every `runner` process is killed before the next entry point starts. The
network is blocked as for `sbox run`, so connections show up as attempts, and
hostname lookups show the names, attributed to the entry point that made them
(see [DNS](#dns)).

| Exit code | Meaning |
|---|---|
| `0` | PASS |
| `1` | REVIEW |
| `2` | BLOCK |
| `125` | sbox failed: bad usage, VM not set up, or the microVM stopped before the scan started |

### Dependencies

Before the scan, a separate prefetch microVM downloads what the skill needs,
with package-manager scripts and source builds disabled:

| What | How |
|---|---|
| npm dependencies | `npm ci` (with a lockfile) or `npm install`, `--ignore-scripts` |
| Python requirements | `pip download --only-binary=:all:` per `requirements*.txt`: wheels only, so no `setup.py` runs |
| `npx` MCP servers | fetched into the npm cache, scripts disabled |
| `uvx` MCP servers | `uv tool install` with builds disabled |

Its only way out is a registry-only proxy: CONNECT tunnels to
`registry.npmjs.org`, `pypi.org` and `files.pythonhosted.org` on port 443, with
no TLS interception. Everything else is refused and reported, including git
and URL dependencies and custom registries set in a skill's `.npmrc`. The
proxy runs in the VM's main network namespace on a unix socket; a relay on
172.16.0.1:3128 inside the run's namespace forwards to it, so the namespace
still has no route out. Both are stopped before the scan's microVM boots.

The scan's microVM gets the downloads on a read-only drive, overlaid with a
writable layer (`node_modules`, the npm and uv caches) and wheels installed
offline. Entry points run with npm and uv in offline mode. The `npm rebuild`
entry point is where dependencies' install scripts (`postinstall` and
friends) run, under strace like everything else; npm itself runs there
without a user config, so a read of `~/.npmrc` comes from a dependency.

The report adds a dependencies section: each step's result, the hosts
fetched from, and refused fetches (`prefetch-refused`, MEDIUM). A failed step
or a prefetch timeout is LOW: the entry points run without it.

### Fake credentials

Every run's home folder holds fake credentials where real ones live:
`~/.aws/credentials`, `~/.ssh/id_ed25519`, `~/.env`, `~/.netrc`, `~/.npmrc`,
`~/.config/gh/hosts.yml`, `~/.docker/config.json`. In `sbox scan`, entry
points also get fake `ANTHROPIC_API_KEY`, `GITHUB_TOKEN` and AWS keys in
their environment. Each value carries a random marker for the run (and the
word `sbox`), so a read shows in the trace and the content is recognised if it
goes anywhere: in a hostname sent to the DNS logger, or on a command line, as
is or hex-encoded, in any case. They exist only inside the throwaway VM.

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
job: prefetch and main microVM), `guest-init.sh` (runs inside each microVM),
`sbox-dns.py` (the DNS logger) and `sbox-proxy.py` (the registry-only proxy and
its relay); both Python scripts have `--selftest`.
