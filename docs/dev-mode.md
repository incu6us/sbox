# Dev mode: build projects with an agent inside the sandbox

## Goal

```
sbox dev ~/work/myapp
```

boots a microVM with the project inside, Claude Code running in it with
your credentials, the network limited to the hosts you listed, a dev
server's port reaching your browser, and your changes coming back to the
host directory when you ask. The host never runs anything the agent or the
project's dependencies execute. At the end, a short summary: hosts
contacted, files changed, commands run.

Scanning (`sbox scan`) stays as is. It is a different use of the same
machinery and is already finished.

## What exists and carries over

| Need | Already built | Where |
|---|---|---|
| Isolated microVM per run, cleaned up afterwards | yes | `sbox-run.sh`, jailer, netns |
| Network allowlist with a log | yes, `host:port` | `sbox-proxy.py`, `guestJob.egress` |
| DNS log | yes | `sbox-dns.py` |
| Project copied in | yes, read-only, 256 MiB cap | `writeRunTar`, `guest-init.sh` |
| Writable drive that survives a phase | yes, the prefetch drive | `deps.ext4` |
| Claude Code in the image | yes, pinned 2.1.296 | `setup.go`, `provision.sh` |
| Secrets from the host | planned, root-only drive file | [config.md](config.md) |
| Config file | planned | [config.md](config.md) |
| GitHub URL as the project source | planned | [github-targets.md](github-targets.md) |
| Audit of what ran | yes, strace plus rules | `scan.go`, `analyze.go` |

## What is missing

1. **A terminal into the guest.** Today stdin carries the tar and the
   serial console is output only. The agent has to be used interactively.
2. **Changes coming back.** The guest can't write to the host; the run
   folder is deleted on exit.
3. **Ports.** A dev server in the guest is unreachable from the host.
4. **State across sessions.** Every run starts from an empty tmpfs:
   `node_modules`, caches, the agent's own session files are lost.
5. **Credentials for the agent**, not fake ones.

## Design

**Terminal: SSH over the tap, not the serial console.** The serial console
has no window size, no resize signal and one stream for everything. The
guest image gets `openssh-server`; the guest init generates host keys into
tmpfs, installs a per-session public key from the drive into the runner's
`authorized_keys`, and starts `sshd` on 172.16.0.2. The host side runs

```
limactl shell -t sbox sudo ip netns exec sbox-<id> ssh -i <key> -t runner@172.16.0.2 claude
```

which gives a real PTY, resize, colours, and `scp`/`tar` over the same
channel for sync. The private key is generated per session on the host and
sent in the tar, root-only, like the nonce; sbox-run moves it to the
jailer's user before use. Nothing listens outside the namespace.

The run itself splits into two `limactl` calls: `sbox-run start <id>`
(reads the tar, builds the drives, boots, prints `READY`, returns, leaving
the VM running under a session folder) and `sbox-run stop <id>` (the
cleanup that today runs on exit). `sbox dev` calls start, attaches, and
calls stop when the terminal session ends or on Ctrl-C twice. A
`sbox ps` lists sessions with their state; `sbox stop <id>` ends one;
stale sessions older than a day are stopped by the next `start`.

**Pause and resume.** `sbox pause <id>` freezes the microVM in place
(Firecracker's `Paused` state: vCPUs stop, memory stays resident, the
helpers in the namespace keep running) and `sbox resume <id>` continues
it. This needs Firecracker's API socket, so `boot` drops `--no-api` and
`sbox-run` talks to `<jail>/run/firecracker.socket` with `curl
--unix-socket`; the same plumbing the snapshot plan needs. What pause
means in practice:

- The attached terminal stays open; keystrokes queue and land on resume.
  An SSH session survives a pause of up to the TCP keepalive window; the
  attach loop reconnects if it didn't. `sbox watch` shows `paused`.
- The guest clock keeps time across a pause: measured, a 20 s pause left
  it matching the host to the second (kvm-clock follows the host), so no
  fix is needed.
- Memory is still held (1 to 4 GiB per paused session), so `sbox ps`
  shows it and a paused session older than a day is stopped by the next
  `start`, like a stale one.
- `dev.pause_after: 30m` pauses a session with no terminal attached and
  no listening port in use for that long; attaching resumes it. Off by
  default.

Pause to disk, so a session survives a Lima VM restart, is a snapshot:
it belongs to [snapshots.md](snapshots.md), which now has the API socket
for free, and is not in this version.

**Project in: copy, as now.** The tar cap rises to 1 GiB and
`.gitignore`d folders are skipped by default (`node_modules`, `.venv`,
`dist`, `target`), since they are rebuilt inside from the dependency
drive. `sbox dev github.com/o/r` uses the GitHub target fetch.

**Project out: on demand, as a diff, then applied.** Inside the guest the
project lives on a writable drive, `work.ext4`, not tmpfs. `sbox sync`
(and the end of a session) runs `tar` over SSH of the files changed since
the copy in (by mtime against a stamp file, then by content hash on the
host for the candidates), shows the diff, and writes it into the host
directory. `.git` is never copied back: the guest gets a copy of the host's
`.git` so the agent can commit and see history, but the host's `.git`
stays the host's, and the result arrives as working-tree changes you commit
yourself, or as a patch `sbox sync -patch` writes. That is the one rule
from the config plan's push section, kept.

Live two-way sync is not in this version. Firecracker has no virtiofs and
the Lima VM has no host mounts by design; a sync on demand plus a sync at
exit is what you get, and the terminal says `[sbox: 12 files changed, run
sbox sync]` whenever the guest's change count moves (a watcher in the
guest writes a count to a file the attach loop polls over SSH every few
seconds).

**Ports.** `ports: [3000, 5173]` in the config. For each, `sbox-run start`
runs a relay in the VM's main namespace listening on `127.0.0.1:<port>`
and forwarding into the run's namespace to `172.16.0.2:<port>` (the
proxy's `relay` mode, reversed: it already bridges a namespace boundary
through a unix socket). Lima forwards the VM's loopback ports to the
host's `localhost` by default, so `http://localhost:3000` in your browser
reaches the dev server. No port is forwarded unless listed.

**State across sessions.** `work.ext4` and the dependency drive persist
under `/var/lib/sbox/projects/<hash of host path>/` in the Lima VM between
sessions; `sbox dev` reuses them, `sbox dev -fresh` discards them, `sbox
clean` removes all. The project copy-in on a reused drive is a sync from
host to guest of files changed on the host since the last session (same
mtime-and-hash logic, other direction), so edits you made on the host
reach the agent. Claude Code's own state (`~/.claude/`, `~/.claude.json`)
lives on `work.ext4` too, so sessions and settings survive.

**Toolchains.** The image has Node, Python 3, uv and Claude Code. Two
layers add the rest:

- `image.packages` in the user config (`golang-go`, `rustc`, `openjdk-21-jdk`,
  `ruby`): `sbox setup` passes the list to `provision.sh`, which adds it
  to `GUEST_PACKAGES` for mmdebstrap. The rootfs stamp includes the sorted
  list, so a change rebuilds the image and nothing else does. Packages
  come from the signed Ubuntu archive only; no URLs.
- Anything installed into the home directory (rustup, `uv python install`,
  nvm, `pip --user`, `go install`) lands on the project's work drive and
  persists between sessions for free. The preflight says the root is
  read-only so this is where tools go.

No per-session tool drive: the two layers cover it.

**Services on the host.** The guest has no Docker and no systemd, and an
app under development needs Postgres or Redis. The first version uses the
services you already run on your Mac: `services: [5432, 6379]` starts a
relay per port that makes the guest's `172.16.0.1:<port>` reach the host's
`localhost:<port>`, the reverse of `ports`. The guest's `/etc/hosts` gets
`host.sbox` for `172.16.0.1`, so `DATABASE_URL=postgres://...@host.sbox:5432/app`
works. Running services inside the guest is a later feature: it needs a
bigger image or a container runtime, and either is real work.

**Raw TCP egress.** The proxy is HTTPS CONNECT only, so `git push` over
SSH, a remote database, or any protocol that doesn't read `HTTPS_PROXY`
fails. For an `egress` entry whose port is not 443 (`github.com:22`,
`db.example.com:5432`), the DNS logger answers that host with the VM's
own address (the answer mode from the shelved fake-services plan, used
for allowed hosts only) and a relay on that port forwards to the real
host. One host per non-443 port in this version; the preflight refuses a
config with two, and the summary logs raw connections like the proxy's
(`EGRESS ALLOW github.com:22`). Entries on 443 keep going through the
CONNECT proxy, unchanged.

**Reattach.** A session outlives its terminal. `sbox dev` on a project
that already has a running or paused session attaches to it and says so
(`attached to session <id>, running 2h10m`), resuming a paused one.
`-fresh` stops it and starts over; two VMs never share a project's drive. Closing
the terminal detaches without stopping; `sbox stop` ends the session, and
`sbox dev` asks once at detach whether to stop or keep it, with keep as
the default for a session that has unsynced changes.

**Resource limits.** The runner gets `RLIMIT_NPROC` 2048 inside the guest
so a runaway build fails there instead of taking the microVM down (a
`pids.max` on the jailer's cgroup would count Firecracker's threads, not
the guest's processes);
`vm.vcpus` and `vm.memory_mib` size the session; `lima` in the user config
sizes the Lima VM (see [config.md](config.md)). The first copy-in of a
large project is slow and capped at 1 GiB; the preflight prints the size
and the cap.

**Preparing the environment: setup and start hooks.** Two lists of
commands in `dev`, run inside the guest as the runner, from the project
directory, with the session's egress, secrets and `dev.env` in place,
before the terminal attaches:

```yaml
dev:
  env:                         # plain variables; secrets stay under secrets:
    NODE_ENV: development
    DATABASE_URL: postgres://app@host.sbox:5432/app
  setup:                       # once per work drive: first session, after -fresh, or when this list changes
    - npm ci
    - cp -n .env.example .env
    - npm run db:migrate
  start:                       # every session, after setup
    - npm run dev &
```

- `setup` runs when the work drive is new, after `-fresh`, or when the
  hash of the list differs from the one stamped on the drive at the last
  successful run. A failed step stops the list, the drive is not stamped,
  and the session still opens with the failure in the preflight output,
  so the fix can be made from inside. `sbox setup-env` (a `sbox dev`
  action, not the global `sbox setup`) reruns it on demand.
- `start` runs at every attach to a fresh boot, not on reattach to a
  running session. A trailing `&` keeps a step running in the background
  (a dev server); its output goes to `sbox log <id>`.
- Both run under `sh -e` with output streamed to the terminal, prefixed,
  and in the session log. A step's timeout is `timeouts.setup`, default
  ten minutes, since `npm ci` on a cold cache is the slow one.
- The project copy-in happens first, then `setup`, then `start`, then the
  terminal. `.mcp.json` servers start with the agent, after the hooks.

The hooks are the project's `sbox.yaml`, so they are yours; the usual
rule that a scan target's file is never read still holds, and `sbox scan`
has no hooks. A dependency install in `setup` is the dev-mode equivalent
of scan's prefetch: it goes through the proxy with the registries allowed,
and its refused hosts show in the summary like any other.

**Credentials.** From the config's `secrets`: `ANTHROPIC_API_KEY` or
`CLAUDE_CODE_OAUTH_TOKEN` (`claude setup-token` on the host prints one),
`GITHUB_TOKEN` scoped to the repositories the agent may push to, and
whatever the project needs. Values go in through the root-only drive file
and into the runner's environment. Everything else that normally lives in
a home directory is still fake and still carries the canary: a dependency
that reads `~/.aws/credentials` during `npm install` is reported, as in
scan mode. No host folder is mounted unless listed under `mounts`.

**Network.** The config's `egress` list plus two implicit sets: the
package registries (needed to install anything) and the agent's API host.
The DNS logger answers NXDOMAIN for everything not in the list, so a
dependency's telemetry fails fast and shows in the summary instead of
hanging. `HTTPS_PROXY` is set for the whole session; tools that ignore it
fail, which is the point. Plain `http://` is refused by the proxy; the
summary names each refused host so you can add it.

**MCP servers and tool permissions.** Two layers, and the config writes
both.

The outer layer is the VM: every tool, built-in or MCP, runs inside it,
behind the egress allowlist, with no host mounts. So the agent runs with
Claude Code's permission prompts off by default (`dev.permissions: skip`,
which is `--dangerously-skip-permissions`; the runner is not root, so it is
allowed), because the prompt would be asking about a throwaway machine.
`dev.permissions: ask` keeps the prompts for people who want them.

The inner layer is Claude Code's own rules, for the cases where the
sandbox isn't the right granularity: a tool that must never run even
inside (`deny`), or a server whose tools are fine except one. sbox
writes `/home/runner/.claude/settings.json` in the guest from the config:

```yaml
mcp:
  servers:                      # merged into the guest's ~/.claude.json
    postgres:
      command: npx
      args: ["-y", "@modelcontextprotocol/server-postgres", "postgres://app:${DB_PASSWORD}@db.internal:5432/app"]
      env: {DB_PASSWORD: env:DB_SBOX_PASSWORD}   # same env: form as secrets
    docs:
      url: https://mcp.example.com/sse           # remote: its host must be in egress
  allow: ["mcp__postgres__query", "mcp__docs__*"]
  deny:  ["mcp__postgres__execute"]
permissions:
  allow: ["Bash(npm run *)", "Bash(git *)"]
  deny:  ["Bash(rm -rf *)", "WebFetch"]
```

What runs where:

- **stdio servers** (`command`) from this file and from the project's
  `.mcp.json` run inside the guest as the runner, under the allowlist, with
  their tokens through the secrets mechanism. Their packages are installed
  by the dependency prefetch, so `npx -y` works offline on the second
  session.
- **remote servers** (`url`) work when their host is in `egress`; the proxy
  log shows them like any other destination.
- **servers on your Mac** (claude.ai connectors, IDE plugins, anything on
  `localhost`) are not reachable from the guest, by design. The summary
  lists servers from `.mcp.json` that could not start, so a missing one is
  visible, not silent.

`deny` entries are written as given; `allow` entries matter only with
`permissions: ask`. `WebFetch` and `WebSearch` are allowed by default and
go through the proxy, so denying them is a choice, not a need.

**Diagnostics: what was blocked and why.** Three moments, one format.
Every blocked event is a line with the thing, the cause, and the config
key that would change it:

```
BLOCKED egress    telemetry.next.dev:443 ×12     not in egress        → egress: [telemetry.next.dev]  (or leave it)
BLOCKED dns       registry.yarnpkg.com (A)       not in egress        → egress: [registry.yarnpkg.com]
BLOCKED mcp       docs                           host mcp.example.com not in egress
BLOCKED tool      mcp__postgres__execute         denied by mcp.deny
BLOCKED port      5173                           guest is listening, not forwarded → ports: [5173]
MISSING secret    DB_SBOX_PASSWORD               host variable unset
MISSING mount     ~/.gitconfig                   no such file on the host
```

*Before:* `sbox dev` runs a preflight and prints the effective
permissions in five lines (egress, ports, mounts, secrets present or
missing, MCP servers and where each will run) before booting. A missing
secret or mount is an error here, not a surprise an hour in. `sbox config`
shows the same without starting anything.

*During:* a watcher in the guest (the one that counts changed files)
also records, once per second, the ports the runner's processes listen on
(`ss -ltn`) and Claude Code's denied tool calls (from the session
transcript under `~/.claude/projects/`, format to confirm in commit 7).
The VM side has the proxy and DNS logs as they happen. `sbox watch [id]`
in a second terminal tails the merged stream, so the agent's terminal
stays the agent's. New kinds of blocked event show there first; the egress
and DNS ones are reliable, the tool-denial one depends on the transcript
format and is marked experimental until it isn't.

*After:* the session summary ends with the BLOCKED and MISSING lines
grouped and counted, and `sbox log <id>` has everything (`-blocked` to
filter, `-f` to follow a running session). A refused host that appears
more than a few times gets the hint line; one that appears once does not,
because telemetry and update checks are usually right to stay refused.

The read-only root is also a cause of failures ("Permission denied" on
`/usr/local/lib/node_modules`), and strace is off by default, so those
can't be reported as events. The preflight says it once: *the root is
read-only; install into the project or `~/.local`*. With `-trace` on,
`EROFS` and `EACCES` results outside `/home/runner` become BLOCKED lines
too.

**Audit.** strace stays off by default in dev mode (it slows builds). The
DNS log, the egress log and the sync diff are always on. `sbox dev -trace`
turns strace on for the session and runs the scan rules over it, with
`INFO` severities and no verdict. The session summary:

```
session 3h12m · 41 files changed (sbox sync to apply) · 2 commits in the guest
egress   api.anthropic.com ×214, registry.npmjs.org ×88, github.com ×3
refused  telemetry.next.dev ×12, o4505.ingest.sentry.io ×2
dns      see sbox log <id>
```

## Config

The file from [config.md](config.md), with `dev` keys:

```yaml
egress: [github.com, "*.githubusercontent.com", "github.com:22"]
services: [5432]
secrets:
  CLAUDE_CODE_OAUTH_TOKEN: env:CLAUDE_CODE_OAUTH_TOKEN
  GITHUB_TOKEN: env:GITHUB_SBOX_TOKEN
ports: [3000]
mounts:
  - host: ~/.gitconfig
    guest: /home/runner/.gitconfig
dev:
  command: claude            # or a shell: bash
  permissions: skip          # or ask
  pause_after: 30m           # idle with no terminal and no port in use; off when absent
  env: {NODE_ENV: development}
  setup: [npm ci]            # once per work drive, or when this list changes
  start: ["npm run dev &"]   # every fresh boot
  vcpus: 4
  memory_mib: 4096
  ignore: [node_modules, .venv, dist]
```

A `sbox.yaml` in the project root is read in dev mode, layered over the
user's `~/.config/sbox/config.yaml` (lists append, scalars replace). The
scan plan refuses a target-side file because the target is untrusted;
here the project is yours. The session summary still lists what the file
granted. The config is the only surface: `sbox dev` takes the directory,
`-config`, `-fresh`, `-command` and `-v`, and nothing else; `sbox init`
writes a commented starter file and `sbox config` prints the effective one.

## Commits

- [x] **1. Start, stop, ps, pause, resume.** `sbox-run.sh` gains
    `start|stop|ps|pause|resume|log` with a session folder per id; the
    one-shot run keeps its positional form (`sbox-run <id> <timeout>`),
    since a hex id never collides with a subcommand. `start` prints
    `READY <id>` and returns with the VM running; `pause` and `resume` go
    through the API socket (`--no-api` dropped in session mode only).
    Host side: `sbox start <dir> -- <cmd>`, `sbox ps|stop|pause|resume|log`.
    Doctor checks that `ps` is empty after a stopped session and that a
    pause-resume round trip keeps a guest counter running.
    - [x] VM side, tested from Go with fakes (`sboxrun_test.go`)
    - [x] host commands, tested with a fake `sbox-run` (`sessions_test.go`)
    - [x] `go test -tags vm -run TestSessionLifecycleOnVM` passes on a real VM
    - [x] doctor's session check
- [x] **2. SSH.** `openssh-server` in the image (`ROOTFS_REVISION` 7),
    with the package's baked host keys removed and `runner` unlocked
    (`*`, not `!`: without PAM, sshd refuses a locked account even for
    keys). `sbox-run` makes the keys in session mode: the client key stays
    root-only in the session folder, the guest gets its public key and a
    host key on the drive, pinned in the session's `known_hosts`. The guest
    init mounts `/dev/pts` and starts `sshd` on 172.16.0.2 (runner only,
    empty config). `sbox-run attach` runs `ssh` in the session's namespace,
    `-t` when stdin is a terminal. Host: `sbox attach <id> [-- cmd]` and
    `sbox dev <dir> [-- cmd]` (a session running `sleep infinity`, a
    terminal in it, stopped on exit).
    - [x] tested from Go with fakes, then on the real VM: output, uid 1000,
      the project folder, the exit code, a `/dev/pts` terminal; through
      `sbox dev`, Ctrl-C reaches the guest program
    - [x] the guest clock: measured instead of fixed. After a 20 s pause it
      matched the host to the second (Firecracker's kvm-clock follows the
      host), so the planned clock fix and its restricted root key are not
      needed
    - [ ] window resize with a real terminal (not testable without one;
      ssh forwards it by design)
- [x] **3. Writable work drive and persistence.** A project id (the
    SHA-256 of the folder's resolved path, 16 hex digits) names a sparse
    20 GiB ext4 drive labelled `sbox-work` under
    `/var/lib/sbox/projects/<id>/`, made on first use. The guest mounts it
    as the runner's home, so the project copy, the agent's state, caches
    and tools installed in the home all persist. The project is copied in
    only when the home has none, so the agent's work is never overwritten;
    host edits reach it with step 4's sync. One live session per project.
    `sbox dev -fresh` and `sbox clean` drop the drives.
    - [x] `stop` flushes first: root has a key on the drive limited to
      `command="sync"`, and a paused guest is resumed for it. Found on the
      real VM: a write just before stop was lost with the guest's page
      cache
    - [x] tested from Go with fakes, then on the real VM (a home file and a
      project edit survive into the next session; after forget, the host's
      copy and an empty home)
- [x] **4. Sync.** `sbox sync [-apply] <dir>`: a three-way merge of the
    Mac's files and the guest's against a base kept on the Mac (path,
    SHA-256, executable bit), recorded when `sbox dev` first copies a
    project in (`sbox-run start` prints `NEW <id>` for a new drive). A
    change on one side goes to the other; different changes on both are
    conflicts, left alone. Dry run by default, with unified diffs;
    `-apply` carries it out and moves the base. Uses the project's running
    session, or starts one and stops it (which flushes). The guest's files
    arrive as a tar of regular files only, unpacked with the safe
    extractor; nothing is written through a symlink on the Mac; symlinks,
    `.git`, `node_modules`, `.venv`, `__pycache__`, `.DS_Store` are never
    synced. Tests on the merge, the manifest, the extraction and the
    writes with fixtures, then on the real VM.
    - [x] `-patch`: write the guest's changes as a patch instead (git's
      `a/` `b/` prefixes, for `git apply` or `patch -p1 -E`; the base
      doesn't move; escaped when printed to a terminal)
    - [x] push the Mac's changes automatically when `sbox dev` reuses a home
      (only the Mac-to-guest half, before the hooks; the guest's own
      changes wait for `sbox sync`, with a line saying how many)
    - [ ] the change-count notice in the terminal
- [x] **5. Ports and services.** From the config: `ports` (guest to the
    Mac's localhost) and `services` (the Mac's localhost to the guest, as
    `host.sbox`). Each is `sbox-proxy.py relay` and `expose` joined by a
    unix socket, one in the session's namespace and one outside; the guest
    gets `host.sbox` in a bind-mounted `/etc/hosts`; a port already
    listening in the VM is refused at start. The config loader comes in
    here with these two keys.
    - [x] tested with fakes and the real proxy script, then on the real VM
      (a guest page on the Mac's localhost; a Mac echo service from the
      guest)
    - [ ] doctor's forwarding check
- [x] **5b. Egress, with raw TCP.** `egress` in the config (a name or
    `*.domain`, `:port` or `:*`, 443 when left out) goes through the HTTPS
    proxy, which now matches those patterns. An exact host on a port other
    than 443 is raw TCP: the DNS logger answers that name with 172.16.0.1
    (an empty answer for AAAA, NXDOMAIN for every other name), and a
    relay/expose pair carries the connection to that host alone; one host
    per raw port. SSH logins get the proxy settings through sshd `SetEnv`.
    - [x] tested with fakes and both Python self-tests (now run by
      `go test`), then on the real VM: `api.github.com` answers,
      `example.com` is refused, `github.com:22` gives GitHub's SSH banner
    - Found on the real VM: inside the namespace unprivileged binds
      started at 53 (for the DNS logger), so the relay couldn't bind 22;
      it is 0 there now, since only sbox's helpers listen in it
- [x] **5c. Reattach and detach.** Session lookup by project hash, attach
    to running or paused, the detach question, `-fresh` semantics.
    - [x] `sbox dev` finds the project's running or paused session in
      `sbox-run ps`, resumes a paused one, and attaches with a note
    - [x] at detach, `keep session <id> running? [Y/n]` when stdin is a
      terminal; without one the session stops, as before
    - [x] `-fresh` stops the live session, then drops the drive: one
      session per project stays the rule, since two would share a drive
    - [x] `sbox sync` resumes a paused session for the sync and pauses it
      again (it used to try a second session and fail on "in use")
    - [x] tested with fakes, then on the real VM (kept, paused, reattached
      to the same guest's process, stopped)
    - [ ] keep as the default only when the guest has unsynced changes:
      the question always defaults to keep for now
- [x] **5d. Toolchains.** `image.packages` through setup and the stamp,
    `pids.max` in the jailer cgroup, the read-only-root line in preflight.
    Probe: `go version` after adding `golang-go`; a fork bomb in the guest
    ends with the session still attached.
    - [x] `image.packages`: Debian names only, user config only, sorted;
      `provision.sh` adds them to mmdebstrap's list and a hash of them to
      the stamp (no list, old stamp: nothing rebuilds)
    - [x] process cap: not `pids.max`, which on the jailer's cgroup counts
      Firecracker's threads, not the guest kernel's processes. The guest
      init sets `RLIMIT_NPROC` 2048 with `prlimit`; root is exempt
    - [x] the read-only-root line, printed by `sbox dev` on a new home
      (the full preflight is step 7)
    - [x] tested with fakes, then on the real VM: `go version` and
      `go run` in the guest, `TestImagePackagesOnVM`,
      `TestGuestProcessLimitOnVM` (red before the cap: 3000 forked)
- [x] **5e. Setup and start hooks.** `dev.env`, `dev.setup`, `dev.start`,
    the hash stamp on the work drive, `sbox setup-env`, `timeouts.setup`,
    streamed and logged output, failure shown in preflight without
    blocking the session. Probe: a project with `setup: [npm ci]` has
    `node_modules` on the second session without rerunning; changing the
    list reruns it; a failing step is visible and the terminal still opens.
    - [x] config: `dev.env` (maps merge, the project's wins; names checked,
      values one line), `dev.setup` and `dev.start` (appended in order),
      `timeouts.setup` (10m when unset)
    - [x] the hook script, run over attach on a fresh boot only, with the
      stamp in `~/.sbox/setup.sha`; tested by running it with `sh`
    - [x] `sbox setup-env <dir>`
    - [x] `dev.env` in every login through `~/.ssh/environment` and
      `PermitUserEnvironment`
    - [x] tested with fakes, then on the real VM (`TestDevHooksOnVM`)
    - [ ] hook output prefixed and in `sbox log`: the runner can't write
      the console, so steps are announced instead and background output
      goes to `~/.sbox/start.log`
- [x] **6. Secrets and config.** The config plan's commits 1, 2 and 4, done
    here if not before, plus `ports`, `dev`, `mcp` and `permissions` keys,
    the generated guest `settings.json` and `.claude.json` MCP entries, the
    project-root file in dev mode, and the "could not start" check for
    `.mcp.json` servers.
    - [x] secrets: `NAME: env:HOST_VARIABLE` (never a literal value, and
      errors never print one), read from the Mac's environment before any
      VM call, a missing one an error; root-only `sbox/secrets` on the run
      drive; the guest init writes `dev.env` then the secrets (a secret
      wins) to a tmpfs file that `~/.ssh/environment` links to, so values
      are never on the kept home. `sbox dev` prints the names. Canary tails
      are scan-mode (`analyze`) and wait for it
    - [x] `dev.command`, `dev.permissions` (skip writes `bypassPermissions`
      and `skipDangerousModePermissionPrompt` to the user settings; ask
      writes `default`), `permissions` and `mcp` (`servers`, `allow`,
      `deny`): `scripts/sbox-claude.py` (with `--selftest`) merges them as
      the runner at boot; sbox owns `permissions` in settings.json and the
      servers it names in `~/.claude.json`
    - [x] `dev.vcpus`, `dev.memory_mib`: `sbox/vm` sizes the main microVM
    - [x] `sbox init` (a starter file, never over one) and `sbox config`
      (the effective file, each secret set or unset, never a value)
    - [x] tested with fakes and the helper's self-test, then on the real VM
      (`TestDevSecretsOnVM`, `TestDevAgentSettingsOnVM`: `claude mcp list`
      shows the servers)
    - [ ] the "could not start" check for `.mcp.json` servers: step 7
    - [x] `dev.ignore`: names or globs at any depth, or paths from the
      root, left out of the copy-in, the base and both sides of sync;
      `node_modules`, `.venv`, `__pycache__` and `.DS_Store` are left out of
      the copy-in by default now (a Mac's `node_modules` has Mac binaries)
    - [ ] `dev.pause_after`, `-config`, `-command`
- [x] **6b. Config changes under a running session.** The config a
    session booted with is recorded on the Mac; attaching again and
    `sbox setup-env` name the keys that changed; `sbox dev -restart`
    reboots on the same home to apply them. Live apply without a reboot
    (permissions, mcp, mounts; egress and ports with a proxy reload) is
    left until restarting proves too slow.
- [x] **7. Diagnostics, summary and `-trace`.** The preflight in `sbox dev`,
    the BLOCKED/MISSING line format with hints, the guest watcher's port
    and tool-denial records (transcript format confirmed here), `sbox
    watch`, `sbox log <id>` with `-blocked` and `-f`, the session summary
    at exit, strace opt-in with INFO rules and EROFS/EACCES as events.
    - [x] `sbox-run events <id>`: allowlist, forwarded ports, proxy and
      DNS logs, from the VM side
    - [x] BLOCKED/FAILED lines with causes, hints from three sightings;
      ports from the guest's `ss -Hltn` (unforwarded, or loopback-only on
      a forwarded port: the relay can't reach those)
    - [x] the preflight in `sbox dev` and `sbox config`, with remote MCP
      servers (config and `.mcp.json`) checked against egress
    - [x] the summary at exit, `sbox watch [id]`, `sbox log -blocked`,
      `sbox log -f`
    - [x] tested with fakes, then on the real VM (`TestDiagnosticsOnVM`)
    - [ ] `-trace`: strace in dev mode with the scan rules at INFO, and
      EROFS/EACCES as events
    - [ ] Claude Code's denied tool calls from the session transcript
      (marked experimental in the design)
- [x] **8. Docs.** README gets a `sbox dev` section first, scan second.
    `MILESTONE.md` gets a dev-mode milestone with timings: boot to prompt,
    sync of a 1,000-file project.
    - [x] README: dev mode and a quick start first; timings in
      `MILESTONE.md` (4.4 s to the command on a new home, 3.2 to 4.0 s on
      a kept one, 3.5 to 3.7 s for a sync that starts its own session)

## Verification

The `vm`-tagged tests cover parts of these (a kept, paused and reattached
session; refused hosts in the summary; forwarded ports and Mac services;
secrets kept off the home); the end-to-end runs below with a real app,
Postgres and a real token are still to do.

- [ ] A Next.js app: `sbox dev`, ask the agent to add a page, `npm run dev`
    in the guest, the page opens on `localhost:3000` on the host, `sbox
    sync` brings the files back, `git diff` on the host shows exactly them.
- [ ] `setup` runs `npm ci` and a migration against the Mac's Postgres
  through `host.sbox`; `start` launches the dev server in the background
  and the page is on `localhost:3000` before the agent prompt appears.
- [ ] A Go project with `image.packages: [golang-go]`: `go build` works in
  the guest; `rustup` installed into the home directory is still there in
  the next session.
- [ ] `psql` in the guest reaches Postgres running on the Mac through
  `host.sbox:5432`; nothing else on the Mac is reachable.
- [ ] Killing the terminal mid-session, then `sbox dev` again: attached to
  the same session, the agent's conversation still there.
- [ ] Second `sbox dev` on the same project starts without reinstalling
    `node_modules`; an edit made on the host in between is visible in the
    guest.
- [ ] `curl https://example.com` in the guest is refused and appears under
    `refused`; `git push` to the allowed repo with the scoped token works.
- [ ] The host's `.git` is byte-identical before and after a session with
    guest commits; `sbox sync -patch` produces an applicable patch.
- [ ] `sbox pause` during `npm run dev`, wait five minutes, `sbox resume`:
  the dev server answers again, `date` in the guest is right, the attached
  terminal still works.
- [ ] `sbox ps` is empty after exit, also after killing the terminal; the
    project's state folder is the only thing left, and `sbox clean` removes it.
- [ ] A session where the agent hits `yarn`'s registry, a denied MCP tool and
  a dev server on an unlisted port: `sbox watch` shows all three as they
  happen, the summary groups them with the key to add, and adding the
  keys makes the next session's preflight show them as allowed.
- [ ] A project `.mcp.json` with a stdio server: the agent lists its tools in
  the guest; a denied tool is refused by Claude Code; a remote server whose
  host is not in `egress` shows under `refused` and in the could-not-start
  list.
- [ ] `~/.aws/credentials` read by a planted-probe dependency during
    install shows in the summary with the canary rule.

## Out of scope for this version

- Live two-way file sync or a mounted project directory.
- Cursor or other agents in dev mode; the `command` key runs anything in
  the image, so a second agent is an image change, not a design change.
- GUI or browser inside the guest; browser automation (Playwright,
  Puppeteer) needs Chromium in the image and is a later `image.packages`
  case once it is tested.
- Services running inside the guest (Postgres in the VM); use the host's
  through `services` for now.
- Sharing one sandbox between projects.
