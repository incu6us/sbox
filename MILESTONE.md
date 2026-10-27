# Milestones

Status: ✅ done · 🟡 partly done · ⬜ not started

| # | Milestone | Status |
|---|---|---|
| 0 | Feasibility spike: Firecracker on Apple Silicon | ✅ |
| 1 | `sbox setup` / `sbox doctor`, pinned downloads, guest image | ✅ |
| 2 | microVM runner: `sbox run` | ✅ |
| 3 | Network: per-run isolation ✅, DNS logging ✅, other fake services ⬜ | 🟡 |
| 4 | Guest instrumentation and skill entry points: `sbox scan` ✅, more entry points ⬜ | 🟡 |
| 5 | Dependency prefetch | ✅ |
| 6 | Analysis and report: findings, verdict, JSON | ✅ |
| 7 | Calibration on installed skills and plugins | ✅ |
| v2 | Agent mode, snapshots, GitHub URLs | 🟡 |
| Dev | Build projects with an agent inside a session microVM: sessions ✅, terminal ✅, kept state ✅, sync ✅ | 🟡 |
| Pre | `sbox setup` installs Lima, host preflight | ✅ |

## Architecture

```
macOS host
└─ sbox CLI ── limactl ──▶ Lima VM "sbox" (vz, nestedVirtualization, Ubuntu 24.04, no host mounts)
                           ├─ /opt/sbox: firecracker, jailer, vmlinux, rootfs.ext4 (read-only, shared)
                           ├─ /opt/sbox/bin/sbox-run: one microVM per call
                           └─ per run: network namespace sbox-<id> ── tap0 ── microVM
                                       jail /srv/sbox-jail/firecracker/<id>, cgroup v2
```

Go, standard library only. VM-side scripts live in `scripts/` and are embedded
in the binary.

---

## 0. Feasibility spike ✅

Commit `4668c9e`. Details in [spike/README.md](spike/README.md).

- Lima 2.2.1 VM with `vmType: vz` and `nestedVirtualization: true` exposes
  `/dev/kvm` on the M5 Pro (macOS 26.7).
- Firecracker v1.17.0 boots a microVM from Firecracker's CI kernel 6.1.155 in
  2.6 s, start to clean exit.
- Learned: Firecracker's serial console reads stdin, so always run it with
  `</dev/null`. CI images lag releases (v1.15). The CI rootfs has no node.

## 1. Setup and doctor ✅

Commits `a9accf0`, `4a12d11`.

- `sbox setup` creates the VM if missing (4 CPU, 6 GiB, 30 GiB disk, no
  mounts), starts it if stopped, provisions it (`scripts/provision.sh`, run as
  root), installs `sbox-run`, then runs doctor. Idempotent.
- Pinned artifacts in `setup.go`, each checked against SHA-256 before use:
  Firecracker v1.17.0, kernel 6.1.155 (sum taken on first download, since CI
  publishes none), Node v24.21.0, uv 0.13.0. A mismatch aborts setup and
  caches nothing.
- Guest image (834 MB): Ubuntu noble minbase via mmdebstrap from the signed
  archive, plus python3, strace, curl, git, iproute2, Node and uv, a `runner`
  user (uid 1000), `/drive`, and two inits: `sbox-hello` (doctor) and the
  `sbox-init` stub. Rebuilt only when `ROOTFS_REVISION` or the pins change.
- Guests power off via sysrq: there's no systemd, so no `reboot(8)`.
- `sbox doctor` checks limactl, the VM, nested virtualization, `/dev/kvm`,
  Firecracker version and checksums, the rootfs, the runner, a hello boot
  (toolchain versions), and a real `sbox run` probe (see milestone 3).
- Learned: group changes don't reach Lima shells (Lima reuses its SSH
  connection), so VM-side Firecracker starts as root, via the jailer.

## 2. microVM runner ✅

Commit `32e9417`.

- `sbox run [-timeout 60s] [-v] <dir> -- <command...>`. Exits with the
  command's status, 124 on timeout, 125 when sbox fails. About 1.4 s per run.
- The host packs the directory (symlinks kept as links, 256 MiB cap), the
  guest init, the command and a nonce into a tar, and streams it to
  `sbox-run <id> <timeout>` in the VM.
- `sbox-run` builds a per-run ext4 drive, hard-links the kernel and the shared
  rootfs into the jail, and starts Firecracker through the jailer: uid/gid
  `sbox-vm`, chroot, cgroup v2. It removes the jail, drive, cgroup and
  namespace on exit, also on timeout.
- In the guest, the read-only root has scratch in tmpfs (`/tmp`, `/var/tmp`,
  `/run`, `/home/runner`). The directory is copied to `/home/runner/work`, and
  the command runs there as `runner` through `setpriv`.
- The `sbox-init` stub in the image only mounts the run's drive and hands over
  to `init.sh` on it, so run logic changes without an image rebuild.
- Integrity:
  - Result markers carry a per-run nonce, stored root-only (0400) on the
    drive, so the command can't print a fake exit status. Tested: a printed
    fake marker is shown as output, and reading the nonce gives "Permission
    denied".
  - A timeout is reported through `sbox-run`'s exit code, not the console,
    which the guest controls.
  - Guest output is shown with control characters escaped.

## 3. Network 🟡

### 3a. Per-run isolation ✅

- Each run gets a network namespace `sbox-<id>` that holds only loopback and
  `tap0` (172.16.0.1/30, owned by `sbox-vm`). The jailer joins it with
  `--netns` before starting Firecracker.
- There's no veth and no route out of the namespace. The guest (172.16.0.2,
  default route via the tap) can reach the VM side of its tap and nothing
  else. The isolation comes from topology, not firewall rules.
- Every run uses the same addresses, with no conflicts: tested with two
  concurrent runs.
- The guest init enables unprivileged ICMP (`ping_group_range`), which
  systemd would normally set.
- Doctor's run probe checks, through a real `sbox run`: the command runs as
  uid 1000, the root is read-only, the VM side is reachable, and the internet
  is blocked.
- Fixed while building: `runVM` takes separate writers for guest output and VM
  diagnostics, because exec copies stderr from its own goroutine and a shared
  `bytes.Buffer` lost data.

### 3b. DNS logging ✅

- `scripts/sbox-dns.py` (Python 3 standard library; the VM has Python, and a
  Go helper would need cross-compiling on the user's machine): answers every
  query with NXDOMAIN and logs type and name, one line per query. Never
  forwards, never returns an address. Malformed, compressed or truncated
  questions are dropped; non-printable label bytes are escaped so a crafted
  name can't forge a log line; at most 10,000 lines.
- `sbox-run` starts it inside the run's namespace on 172.16.0.1:53 as
  `sbox-vm` (port 53 allowed by a per-namespace sysctl), with its stdio
  detached. After the microVM exits it prints the log marked with the
  run's nonce, read from the drive it built.
- The guest init bind-mounts a `resolv.conf` pointing at 172.16.0.1 (timeout
  1 s, 1 attempt) from tmpfs over the read-only one.
- `sbox scan` looks up `entry-N-<nonce>.sbox.invalid` before each entry point
  and attributes the lookups that follow to it; entry points can't forge a
  sentinel without the nonce. The report shows `DNS <name> (<types>) →
  NXDOMAIN` in place of strace's anonymous DNS lines.
- `sbox run` prints the names in its summary. Doctor's probe checks that a
  lookup reaches the log.
- Fixed while building: the logger was started through a shell function, so
  cleanup killed the wrong PID, and the surviving logger held stdout open and
  hung the run. It now starts directly with detached stdio, and cleanup
  kills every process left in the namespace before deleting it.

### 3c. Other fake services ⬜

Services inside each run's namespace that answer the guest's network
requests without anything leaving the machine, so a run can show which
destinations the skill tried to reach. To be scoped and reviewed as its own
step before implementation.

Evidence from milestone 4 led to 3b: strace shows connection attempts to
IP addresses with their destination, but a hostname's name travels in the
packet, not the syscall. Anything beyond DNS logging (answering lookups,
accepting connections) stays out until a scan shows it's needed.

## 4. Guest instrumentation and skill entry points 🟡

### 4a. `sbox scan` ✅

- `entrypoints.go` finds the automatic entry points: hook commands (under
  `hooks`, keyed by event, in Claude Code's and Cursor's layouts), stdio MCP
  servers (`mcpServers`), root `package.json` install scripts and `setup.py`.
  `node_modules` and `.git` are skipped.
- Hooks get a realistic event payload on stdin (session id, cwd, event name,
  plus `tool_name`/`tool_input`, `prompt` or `source` per event). MCP servers
  get `initialize` and `tools/list`, and stdin stays open until the timeout.
- `runVM` now takes a `guestJob`: a command run as the runner, or a scan script
  run as root plus input files (`sbox/in/`, readable by the runner). In scan
  mode the guest init runs `sbox/scan.sh` as root with the nonce.
- `scan.sh` runs each entry point with a clean environment, as `runner`,
  under `strace -f -u runner` (execve, connect, openat, unlink, rename), with a
  per-entry timeout. Traces go to the root-only `/run/sbox`, so an entry
  point can't edit them. Every runner process is killed before the next
  entry point starts.
- The guest sends filtered traces back as nonce-marked `TRACE` lines (system
  paths dropped, 400 lines per entry). The host turns them into processes
  (successful execve only, so PATH lookups don't count), network attempts
  (`host:port`, `unix:path`, labelled DNS lookups) and file operations, and
  prints a report per entry point.
- Verified on the installed ponytail plugin (2 hooks, 6.5 s for the whole
  scan): node started on its own scripts, reads of its own files and config,
  one marker file written under `~/.claude`, no network.
- Verified with a probe hook: a raw-IP connection shows its destination, but a
  hostname shows only as a DNS lookup (see 3b).

### 4b. Still to do ⬜

- Cursor `beforeShellExecution`-style hooks need their own payload fields
  checked against Cursor's docs.
- Run the scripts a skill's `SKILL.md` tells the agent to run (`scripts/`),
  with representative arguments.

## 5. Dependency prefetch ✅

- `sbox-run` runs two microVMs when the run carries `sbox/prefetch.sh`: a
  prefetch VM with an empty writable 4 GB sparse drive, then the main VM with
  that drive read-only. Same namespace, tap and DNS logger.
- Egress for the prefetch VM only: `scripts/sbox-proxy.py serve` (VM main
  namespace, unix socket, as `sbox-vm`) tunnels CONNECT to
  `registry.npmjs.org`, `pypi.org`, `files.pythonhosted.org` on 443, no TLS
  interception, logs ALLOW/DENY; `relay` mode inside the run's namespace
  forwards 172.16.0.1:3128 to the socket. Both are killed before the main VM
  boots. The proxy log is printed with the nonce after the prefetch VM exits.
- `prefetch.go` plans the steps: `npm ci`/`npm install` with scripts off (if
  dependencies aren't bundled), `pip download --only-binary=:all:` per
  requirements file, `npm exec` for npx MCP packages, `uv tool install` with
  builds off for uvx. The steps run as the runner through the proxy.
- The guest init reads `sbox.phase` from the kernel command line. Main phase:
  overlays `node_modules`, the npm cache and the uv cache (read-only drive plus
  tmpfs upper layer, since `npm rebuild` writes), installs wheels offline.
  Entry points run with npm and uv offline, npm's update notifier, audit and
  fund off, and Node's compile cache off.
- A new `npm rebuild` entry point runs dependencies' install scripts under
  strace. npm runs there without a user config, so it doesn't read the fake
  `~/.npmrc` token: the first test run flagged npm's own config read and
  update check as CRITICAL; with that fixed, a read of `~/.npmrc` there comes
  from a dependency.
- Report: a dependencies section (steps, fetched hosts, refused fetches as
  `prefetch-refused` MEDIUM, failures and timeouts LOW), and `prefetch` in JSON.
- Verified: a skill with `is-number` and `six==1.16.0` prefetches both (only
  the three registry hosts contacted), its hook imports both, PASS; a skill
  with a `github:` dependency has the fetch refused and reported, REVIEW. No
  helpers, namespaces, jails or run folders left afterwards.

## 6. Analysis and report ✅

- Trace lines are parsed into typed events (process, connect, DNS, file
  read/write/delete/rename with path and result) instead of display strings,
  so rules look at fields.
- `analyze.go` turns each entry point's events into findings: credential
  reads (even of missing files), connection attempts to non-local addresses,
  writes to agent configuration or persistence paths, programs run from temp
  folders (HIGH); hostname lookups, network tools, sudo/su, `/proc/*/environ`
  reads (MEDIUM); other writes under agent folders and timeouts (LOW). A
  credential read plus any network activity in the same entry point is
  CRITICAL.
- Verdict and exit codes as skills-audit: BLOCK 2, REVIEW 1, PASS 0; 125
  when sbox fails. `-json` prints the whole report.
- The guest no longer drops all of `/proc` as noise, only known-noisy files,
  so reads of a process's `environ` show.
- Verified: ponytail PASS (one low: its marker file under `~/.claude`), the
  network probe REVIEW, and a probe hook reading `~/.aws/credentials` then
  looking up a host BLOCK with exit 2.
- Fake credentials: the host generates a random marker per run (root-only on
  the drive); the guest init plants fake AWS, SSH, `.env`, `.netrc`, npm, gh
  and docker credentials carrying it, and `sbox scan` adds fake
  `ANTHROPIC_API_KEY`, `GITHUB_TOKEN` and AWS keys to the entry points'
  environment. The marker (as is or hex-encoded, any case) in a DNS lookup or
  in a network tool's arguments is CRITICAL (`canary-exfiltration`); on another
  program's command line, HIGH. Verified with a probe hook that read the fake
  AWS secret and put it in a lookup: BLOCK, exit 2; ponytail still PASS.

## 7. Calibration ✅

Scanned all 73 installed plugin and skill folders from
`skills-audit -installed`, plus the cached copies of three uninstalled plugins
that ship real hooks.

- Only ponytail, among the installed ones, declares local entry points;
  figma and vercel use remote (HTTP) MCP servers, which can't run locally.
  The other targets return "no automatic entry points" without starting a VM.
- Results over 4 plugins and 12 entry points:

  | Plugin | Verdict | Notes |
  |---|---|---|
  | ponytail | PASS | one low: its own marker file under `~/.claude` |
  | claude-obsidian | PASS | 4 hooks |
  | claude-seo | PASS | 1 hook |
  | vercel-plugin | REVIEW | a SessionStart hook looks up `telemetry.vercel.com`: telemetry on every session start |

  No false positives, no timeouts; the vercel finding is the kind static
  review misses.
- Fixed from what the run showed:
  - Tool-hook payloads follow the hook's matcher (`Edit|Write` → an `Edit`
    call with a `file_path`), instead of always `Bash`, so hooks take their
    real path.
  - JSON's `network` list matches the text report: when the DNS logger has the
    name, strace's anonymous DNS line is dropped.
- Not scanned: the user's own hooks in `~/.claude*/settings.json` and
  `~/.cursor/hooks.json`. `sbox scan` copies a folder into the VM, and those
  folders hold session transcripts and caches; their hooks also call host
  binaries by absolute path, which don't exist in the guest. A `-file` mode
  that copies a single config file is the way to cover them if needed.

## v2 🟡

- Agent mode (Claude Code first, then Cursor), built in small steps:
  - ✅ Step 1: Claude Code 2.1.296 in the guest image. The linux-arm64 npm
    platform package (just the native `claude` binary, no install scripts) is
    pinned by URL and SHA-256 (its sha512 matches the registry's integrity
    value) and extracted to `/usr/local/bin`. Doctor checks the version;
    `claude --version` runs as the runner in `sbox run`. Image: 1151 MB.
  - ✅ Step 2: process attribution. strace writes one file per process
    (`-ff`), so concurrent syscalls no longer split into unparseable
    "unfinished/resumed" fragments, and traces clone/fork/vfork. The host
    rebuilds the process tree and gives every event the program its process
    was running (inherited from the parent at clone time until it execs;
    for node/python, the script). Entry points can name harness programs
    whose own events aren't judged, only what they launch: `npm rebuild`
    names npm (16 of its own events hidden on the test skill). Hooks name
    none, so their verdicts are unchanged (ponytail PASS, probes as before).
  - ✅ Step 3: egress for the main microVM. A run can declare `host:port`
    entries (`sbox/egress` on its drive); `sbox-run` then starts the same
    allowlist proxy and relay as prefetch for the main microVM, with exactly
    those hosts, and prints the log with the nonce afterwards. The guest sets
    `HTTPS_PROXY` for the command only then. `sbox run -egress` exposes it.
    Verified: with `api.anthropic.com:443` allowed, that host answers and
    example.com is refused; without it, nothing is reachable; prefetch
    unchanged; nothing left behind.
  - Step 4, in pieces:
    - ✅ 4a: `transcript.go` parses Claude Code's `--output-format stream-json`
      (init model, `tool_use` blocks with a one-line summary of each input,
      failed `tool_result`s, the final result's status, turns and text) and
      renders the report's agent section. Malformed lines are skipped and
      output is escaped; the transcript comes from inside the guest, so it
      informs the report but never decides the verdict. Tested on a saved
      sample (`testdata/claude-stream.jsonl`); not wired into `scan` yet.
    - ⬜ next: the agent entry point (Claude Code headless as the runner,
      `claude` as its harness, egress to the API host), the user's
      credential from the host environment, and wiring the transcript in.
  - ⬜ Then a Cursor profile.
- Firecracker snapshots for sub-second starts.
- GitHub URLs as targets (v1: fetch locally first).

## Dev mode 🟡

The direction since 2026-10-27: sbox is for building projects with an
agent inside the sandbox. Plans with checkboxes: [docs/](docs/README.md).

- ✅ Step 1: sessions. `sbox start <dir> -- <cmd>` runs the same job as
  `sbox run` in a microVM that keeps running, and prints its id once the
  guest is up; `sbox ps`, `stop`, `pause`, `resume`, `log`. `sbox-run`
  keeps the run on disk and runs itself on it in session mode, in its own
  process group; Firecracker gets an API socket, and pause and resume
  `PATCH /vm`. Stop sends TERM to the group, which reaches every helper,
  then tears down what is left. Sessions older than a day are stopped by
  the next start.
  - Tested from Go with fake commands on `PATH`, then on the real VM:
    doctor's session probe (a guest counting once a second: 1→3 running,
    3→3 paused, 3→6 resumed, gone after stop) and
    `go test -tags vm -run TestSessionLifecycleOnVM` (27 s) pass; nothing
    left in the VM afterwards; ponytail scan still PASS.
  - Fixed from the real run: the serial console sends CR LF, which the
    fakes didn't; `sbox log` now drops the CR, as `sbox run` already did.

- ✅ Step 2: a terminal in the guest. `sbox dev <dir> [-- cmd]` starts a
  session, attaches, and stops it on exit; `sbox attach <id> [-- cmd]`
  attaches to a running one. SSH from inside the session's namespace:
  `sbox-run` makes a client key (root-only in the session folder) and a
  guest host key (on the drive, pinned in `known_hosts`); the guest runs
  `sshd` on its tap address for the runner only. Image: `openssh-server`,
  1162 MB.
  - Verified on the real VM: output, uid 1000, the project folder and the
    exit code come back; a `/dev/pts` terminal; Ctrl-C through `sbox dev`
    reaches the guest program; nothing left after exit.
  - Fixed from the real runs: `runner` was locked (`!`), and without PAM
    sshd refuses locked accounts even for keys; nothing mounted
    `/dev/pts`, so no terminal could be allocated; doctor's first counter
    read could come before the first tick.
  - Measured, not built: the guest clock matched the host to the second
    after a 20 s pause, so the planned clock fix is dropped.

- ✅ Step 3: state kept per project. `sbox dev` names the folder by the
  SHA-256 of its resolved path and gives the session that project's work
  drive (sparse 20 GiB ext4, labelled `sbox-work`, made on first use), which
  the guest mounts as the runner's home. The project is copied in only when
  the home has none. One live session per project; `sbox dev -fresh` and
  `sbox clean` drop the drives.
  - Found on the real VM: stop killed the microVM with writes still in the
    guest's page cache, so the last seconds of work were lost. Stop now
    flushes first, over a root key on the drive limited to
    `command="sync"`, resuming a paused guest for it.
  - Verified on the real VM: a home file and a project edit survive into
    the next session; after forget, the host's copy and an empty home;
    `sbox dev`, `-fresh` and `sbox clean` through the binary.

- ✅ Step 4: `sbox sync [-apply] <dir>`. A three-way merge of the Mac's
  files and the guest's against a base kept on the Mac (path, SHA-256,
  executable bit), recorded when `sbox dev` first copies a project in. One
  side's change goes to the other; different changes on both are
  conflicts, left alone. Dry run with diffs by default. The guest's files
  arrive as a tar of regular files only, through the safe extractor, and
  nothing is written through a symlink on the Mac. Never synced: symlinks,
  `.git`, `node_modules`, `.venv`, `__pycache__`, `.DS_Store`.
  - Verified on the real VM: edits, an addition and a deletion in the
    guest, an addition on the Mac; the dry run lists all four with the
    diff, `-apply` brings them across, a second run finds nothing.
  - Left for later: `-patch`, pushing Mac edits automatically when
    `sbox dev` reuses a home, a change-count notice in the terminal.

## Prerequisites ✅

- `sbox setup` checks the Mac first (Apple M3 or later, macOS 15 or later,
  10 GiB free) and installs Lima 2.2.1 into `~/.local/share/sbox/lima`
  when no `limactl` is found: pinned SHA-256 from the release's
  `SHA256SUMS`, checked before unpacking; the extractor refuses paths and
  links that leave the folder. Every `limactl` call prefers that copy.
- Verified on an M4 Pro, macOS 26.6.2, with no Lima installed: setup
  installed it, created the VM, built the image (1151 MB) and doctor
  passed. Real disk use: about 4 GiB (Lima's VM disk is sparse).

## Known limits

- Only code paths that actually run are observed. Code that waits, checks
  the date, or looks for a sandbox can stay quiet.
- The guest is arm64 only: x86 binaries in a skill won't run.
- Linux userland, so macOS-specific behaviour is only partly reproducible.
