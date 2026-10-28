# sbox

Build projects with an AI agent inside a Firecracker microVM on your Mac:
the agent gets a real terminal, your project, the tools it needs and the
hosts you allow, and nothing else of your machine. `sbox dev` is the main
command; `sbox scan` still runs a skill's code under strace and reports
what it tries to do. Work in progress: see [MILESTONE.md](MILESTONE.md)
for what's done and what's next.

```sh
sbox setup                          # once: Lima, Firecracker, the guest image
cd myapp && sbox init               # a starter sbox.yaml: claude, its API host
claude setup-token                  # once, on the Mac; export the token it prints:
export CLAUDE_CODE_OAUTH_TOKEN=...  #   and add it under secrets: in sbox.yaml
sbox dev .                          # Claude Code in a microVM on a copy of myapp
sbox sync -apply .                  # bring its changes back to the Mac
```

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
sbox run [-config <file>] [-workspace <name>] [-v] [-refetch] [-apply | -patch <file> | -push -m <message>] <dir | GitHub URL> -- <command> [args...]
sbox scan [-config <file>] [-v] [-json] [-refetch] <dir | GitHub URL>

sbox init [dir]
sbox config [-config <file>] [-workspace <name>] [dir]
sbox dev [-fresh | -restart] [-config <file>] [-workspace <name>] [-command <cmd>] [-trace] <dir | GitHub URL> [-- <command> [args...]]
sbox setup-env [-config <file>] [-workspace <name>] <dir>
sbox start <dir> -- <command> [args...]
sbox attach <session-id> [-- <command> [args...]]
sbox sync [-apply | -patch] [-config <file>] [-workspace <name>] <dir>
sbox ps
sbox stop|pause|resume <session-id>
sbox log [-f] [-blocked | -start] <session-id>
sbox watch [session-id]
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
| `session: a guest port reaches the Mac` | the same guest serves a page on a forwarded port, and the Mac fetches it from `localhost` |

Each failure says what to do (usually `sbox setup`). Exit codes: `0` all
checks pass, `1` a check failed, `2` bad usage.

### `sbox dev` and sessions

`sbox dev <dir>` opens a shell in a microVM with the directory copied
in, at `/home/runner/work`. Pass a command to run that instead:
`sbox dev ./myapp -- claude`. While you work, the terminal's title counts
the guest's changes not yet synced (`sbox: 3 changes to sync`). When you
exit, sbox asks whether to keep the session running: keep is the default
when there are changes to sync, stop when there are none; a kept or paused session is what the next
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

**Workspaces: the project asks, your file grants.** `egress`, `services`,
`mounts` and `secrets` reach past the VM, so a project's `sbox.yaml` can
only ask for them; a repository you just downloaded can't give itself your
`~/.ssh` or the network. Your own file grants them: its top-level keys for
every project, and named workspaces for the projects in a folder:

```yaml
# ~/.config/sbox/config.yaml
egress: [api.anthropic.com]                   # every project
secrets: {CLAUDE_CODE_OAUTH_TOKEN: "env:CLAUDE_CODE_OAUTH_TOKEN"}
workspaces:
  client-a:
    paths: [~/work/client-a]                  # projects under here are in client-a
    egress: ["*.client-a.com"]                # every client-a project gets these
    mounts: [{host: ~/.config/client-a/gitconfig, guest: /home/runner/.gitconfig}]
    allow:                                    # what a client-a project may ask for
      egress: [registry.npmjs.org, "*.github.com"]
      services: ["5432"]
      mounts: [~/work/client-a/shared]        # read-only; rw: [...] for read-write
      secrets: [CLIENT_A_NPM_TOKEN]           # Mac variables it may read
  personal:
    paths: [~/src]
    allow: {egress: ["*"], services: ["*"]}   # your own code: any host or port
```

A project belongs to the workspace whose `paths` hold the deepest folder
containing it, never by its own say; one outside every workspace is in
`default`, which allows nothing more unless your file gives it an `allow`.
`-workspace <name>` picks one for a single command. A request outside the
allowance stops `sbox dev` before anything boots, one line each with the
key to add:

```
BLOCKED mount   ~/.ssh → /home/runner/.ssh  not allowed in workspace default → workspaces.default.allow.mounts: [~/.ssh]
BLOCKED egress  evil.example:443  not allowed in workspace default → workspaces.default.allow.egress: [evil.example]
```

In a terminal it then offers to allow them for that project alone and
remembers the answer (asking again only for something new; `sbox clean`
forgets). Under a running session a refused request is left out and the
session keeps what it booted with. `sbox run` takes the target folder's
workspace grants; `sbox scan` reads no project file and ignores workspaces.

Egress entries on port 443 (or `:*`) go through an HTTPS proxy that
tunnels to the listed hosts only; the guest's `HTTPS_PROXY` points at it,
in the command and in every terminal. An exact host on another port, like
`github.com:22`, is raw TCP: the guest's lookup of that name is answered
with the VM side of its network, where a relay carries the connection to
that host alone, so one host per such port.

The environment is prepared from the same file, inside the guest, as the
runner, from the project folder, before the terminal opens:

```yaml
dev:
  env:                    # in every login: the hooks, terminals, the agent
    DATABASE_URL: postgres://app@host.sbox:5432/app
  setup:                  # once per home, and again when this list changes
    - npm ci
    - npm run db:migrate
  start:                  # at every fresh boot, not when attaching again
    - npm run dev &       # a trailing & runs it in the background (output in ~/.sbox/start.log); without it, a step still running after 30s says so
timeouts:
  setup: 10m              # each step; 10m when left out
```

A failed step stops the list and is shown, and the terminal opens anyway
so you can fix it there; `sbox setup-env <dir>` runs the setup steps
again. `dev.env` is written to the runner's `~/.ssh/environment` at each
boot.

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

A session takes the config when it boots. Change `sbox.yaml` under a
running session and the next `sbox dev` applies what it can right there:
`egress` (the proxy rereads its rules; open connections stay), `ports`
and `services` (their relays start and stop), `permissions`, `mcp` and
`mounts` (Claude Code reads them the next time it starts), all without
stopping anything. Raw TCP egress (a port other than 443), egress for a
session that started without any, and the rest say so
(`the config changed since session <id> started (dev.vcpus)`); for those,
`sbox dev -restart <dir>` stops the session and boots a new one with the config as
it is now, on the same home: files, installed tools and Claude Code's
history stay, running processes don't. `dev.command` applies at every
attach (`-command <cmd>` runs another for one attach, as a `bash -lc`
line); `image.packages` needs `sbox setup`.

Running Claude Code in the guest takes a token from your Mac and its API
host in egress, both in your own file (`sbox init` writes a starter
`sbox.yaml` and prints the two lines). `sbox config` prints the effective
config: the workspace and why, each key commented with where it came from
(`# user`, `# workspace client-a`, `# project`), which secrets are set on
this Mac, what the project asks for that would be refused, and what you
approved for it:

```yaml
dev:
  command: claude         # what sbox dev runs when given no command
  permissions: skip       # the default: no prompts, the VM is the boundary; or ask
  pause_after: 30m        # pause a session nothing is connected to; sbox dev resumes it
  vcpus: 4                # the session's size (2 and 1024 MiB by default)
  memory_mib: 4096
egress: [api.anthropic.com]
secrets:                  # NAME: env:<variable on your Mac>, read at start
  CLAUDE_CODE_OAUTH_TOKEN: env:CLAUDE_CODE_OAUTH_TOKEN   # claude setup-token prints one
permissions:              # Claude Code's own rules in the guest
  deny: ["Bash(rm -rf *)"]
mcp:
  servers:
    pg: {command: npx, args: ["-y", "@modelcontextprotocol/server-postgres", "postgres://app@host.sbox/app"]}
    docs: {url: "https://mcp.example.com/mcp"}   # its host must be in egress
  deny: ["mcp__pg__execute"]
```

Files from the Mac go in as copies, refreshed at every boot, so the Mac's
version wins; nothing comes back (the project itself comes back with
`sbox sync`):

```yaml
mounts:
  - {host: ~/.gitconfig, guest: /home/runner/.gitconfig}
  - {host: ~/.config/gh, guest: /home/runner/.config/gh}   # a real credential replaces the fake one
```

A missing host path stops `sbox dev` before anything starts. Guest paths
are under `/home/runner`, outside the project.

A secret's value is never written in a file: a missing variable stops
`sbox dev` before anything starts, the value travels root-only to the
guest, and it is never on the kept home. What sbox prints from a session
(the summary, `sbox log`, `sbox watch`) has each secret, and its hex and
base64 forms, replaced by `[redacted:NAME]`, so a token echoed into a DNS
lookup doesn't land in your scrollback. An MCP server inherits the
secrets, so a token it needs goes under `secrets`. sbox owns the
`permissions` key of the guest's `~/.claude/settings.json` and the servers
it names in `~/.claude.json`; everything else Claude Code keeps there
survives. A project's `sbox.yaml` is yours, like the code: it can grant
egress and ask for secrets, so read one you didn't write before running
`sbox dev` on it.

Nothing else crosses: the guest's network still has no route out, and
each forwarded port is a pair of relays joined by a unix socket in the
Lima VM. Ports must be 1024 or higher, and free in the VM (a second
session forwarding the same port is refused).

What a session may do is printed before it boots, and what it was kept
from when you leave it. One line per blocked thing, with its cause and,
for a host seen three times or more, the key that would allow it:

```
egress   api.anthropic.com:443
ports    3000 → the Mac's localhost:3000
secrets  CLAUDE_CODE_OAUTH_TOKEN
...
session 1h12m · sbox sync ./myapp shows what changed
egress   api.anthropic.com:443 ×214, registry.npmjs.org:443 ×88
BLOCKED egress  telemetry.example.com:443 ×12  not in egress → egress: [telemetry.example.com]
BLOCKED dns     registry.yarnpkg.com (A, AAAA) ×3  not in egress → egress: [registry.yarnpkg.com]
BLOCKED port    3000  listening on loopback only, the forward can't reach it → listen on 0.0.0.0 (e.g. --host 0.0.0.0)
BLOCKED port    5173  guest is listening, not forwarded → ports: [5173]
BLOCKED mcp     docs  host mcp.example.com:443 not in egress → egress: [mcp.example.com]
BLOCKED mcp     pg  could not start: pg-mcp isn't in the guest → image.packages, or install it into ~/.local
BLOCKED tool    mcp__pg__execute ×2  denied by a permission rule (permissions.deny or mcp.deny)
BLOCKED service host.sbox:6379  not in services → services: [6379]
BLOCKED connect 140.82.112.3:443  straight to an address: the guest goes out only by name, through egress
```

`sbox dev -trace` runs the terminal's command, and `dev.setup` when it
runs (a dependency's install script, say), under strace and ends with
what scan's rules make of it, as INFO lines (setup's marked `(in
dev.setup)`), plus every write the sandbox refused (`BLOCKED write
/usr/local/lib/x  EROFS: the root is read-only → install into ~/.local
or the project`).

`sbox watch [id]` shows these as they happen, in a second terminal;
`sbox log -blocked <id>` lists them for a running session,
`sbox log -f <id>` follows its console, and `sbox log -start -f <id>`
follows what `dev.start`'s background steps print (the dev server). A remote MCP server whose host
isn't in egress, in the config or the project's `.mcp.json`, is flagged
before the session boots.

`sbox sync <dir>` shows what changed in the guest and on the Mac since
they last agreed, with diffs; `sbox sync -apply <dir>` brings each side's
changes to the other; `sbox sync -patch <dir> > guest.patch` writes the
guest's changes as a patch for `git apply` (or `patch -p1 -E`) instead.
When `sbox dev` boots a kept home it copies your Mac edits in first, so
the agent starts from them; its own changes still wait for `sbox sync`. A file changed differently on both sides is listed
as a conflict and left alone. Never synced: symlinks, `.git` (the Mac's
is copied over the guest's at every boot, so the guest's history follows
yours and its own commits last one session: commit the synced files on
the Mac),
`node_modules`, `.venv`, `__pycache__`, `.DS_Store`, what the project's
root `.gitignore` lists, and what `dev.ignore` lists (`[dist, "*.log",
build/cache, "!.env.example"]`: a name or glob at any depth, a path from
the project root, `/name` for the root alone, `!` to take one back).
`sbox dev` doesn't copy those in either, except `.git`, so the agent has
your history; dependencies are installed in the guest, by `dev.setup` for
instance. A `.env` your `.gitignore` lists stays on the Mac: the preflight
names what `.gitignore` left out, and `dev.ignore: ["!.env"]` brings one
in (or use `secrets`).

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

A GitHub URL works wherever a folder does: `https://github.com/o/r`,
`.../tree/<ref>` or `.../tree/<ref>/<folder>`, or the short
`github.com/o/r[@ref][/folder]`. The tarball is downloaded without git
(so no hooks or filters run on the Mac), unpacked into
`~/Library/Caches/sbox/targets/`, and its commit is printed. `sbox dev
<url>` copies it into a new `./<repo>` folder and works there.

### `sbox run`

```sh
./sbox run ./some-skill -- node --version
./sbox run -config ci.yaml ./some-skill -- sh -c 'ls; python3 x.py'
./sbox run -v ./some-skill -- npm test          # also kernel and Firecracker output
```

Runs `<command>` on a copy of `<dir>` in a fresh microVM, in about 1.4 s plus
the command's own time. The `--` is required: everything after it is the
command, passed through exactly (quotes, spaces and `$` arrive intact).

| Flag | Meaning |
|---|---|
| `-config <file>` | A config file read after `~/.config/sbox/config.yaml`. A file inside the target is never read. |
| `-v` | Also show kernel and Firecracker output, marked `·`. Useful when a run fails to start. |
| `-refetch` | Download a GitHub target again, even a commit already in the cache. |
| `-apply` | After the command, show what it changed in the folder (sync's diffs) and write it there once you answer `y`. Needs a terminal, and a folder, not a GitHub URL. |
| `-patch <file>` | Write what the command changed to `<file>` as a patch instead (`git apply`, `patch -p1`). |
| `-push -m <message>` | As `-apply`, then commit just those files with the message and push the branch, with your own git and credentials. The branch's upstream must be listed under `push:` in the config (`push: [github.com/org/repo]`); that's checked before the VM starts and again before the push. |

With either, the guest's copy of the folder comes back after the command,
without `.git`, `node_modules` and the like or what the folder's
`.gitignore` lists; files you changed on the Mac meanwhile are left alone.
`.git` from the guest is never used.

`mounts` in the config are copied in for `run` too. One with `mode: rw`
comes back with `-apply` like the folder, shown under its own heading and
written to its Mac path under the same confirmation; `-patch` and `-push`
cover the folder alone.

The rest comes from the config:

```yaml
egress: [api.github.com, "*.npmjs.org"]   # through a CONNECT proxy (HTTPS_PROXY is set); nothing else gets out
timeouts: {run: 5m}                       # 1m when unset
vm: {vcpus: 4, memory_mib: 4096}          # 2 and 1024 when unset
```

The summary lists allowed and refused connections.

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
./sbox scan -v ./some-skill                    # show the entry points' output
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
(`target` as `{"url", "commit", "path"}` for a GitHub URL or `{"path"}`
for a folder, `verdict`, `counts`, and per entry point `kind`, `name`, `source`,
`command`, `ran`, `exit_code`, `processes`, `network`, `files`, `dns`,
`findings`).

| Flag | Meaning |
|---|---|
| `-config <file>` | A config file read after `~/.config/sbox/config.yaml`; never one from the target. |
| `-v` | Also show the entry points' own output. |
| `-json` | Print the report as JSON. |
| `-refetch` | Download a GitHub target again, even a commit already in the cache. |

From the config: `timeouts.entry` (each entry point, `20s`),
`timeouts.prefetch` (the dependency download, `3m`), `prefetch.enabled`
(`false` skips it), `prefetch.registries` (private registries the download
may reach besides npm and PyPI), `vm.vcpus` and `vm.memory_mib`, and the
calibration:

```yaml
rules:
  expected_hosts: [telemetry.vercel.com, "*.sentry.io"]  # their lookups drop to INFO, marked (expected)
  severity: {agent-dir-write: INFO}       # per rule; the canary rules can't be changed
  credential_paths: [~/.config/myapp/]    # read as credentials, like ~/.aws
  block_on: CRITICAL                      # BLOCK from here (the default)
  review_on: MEDIUM                       # REVIEW from here (the default)
report:
  format: json                            # like -json
  max_listed: 15                          # per list (the default)
  fail_on: block                          # exit 0 on REVIEW; non-zero on BLOCK only
```

`secrets` (`GITHUB_TOKEN: env:SBOX_TOKEN`, read from your environment)
puts real values in front of the entry points, in place of the fake ones
of the same name, to see what a skill does with a real token. Every line
from the guest has them taken out before the report sees it (`[redacted:
GITHUB_TOKEN]`, hex and base64 forms too); one in a lookup or a network
tool's arguments is `canary-exfiltration`, CRITICAL. `sbox run` takes
`secrets` the same way.

Every override that changes a verdict is printed under the header
(`config: expected hosts: telemetry.vercel.com`) and in JSON under
`config.overrides`, so a PASS with a downgraded rule says so.

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

Every `run` and `scan` home folder holds fake credentials where real ones
live (not a `dev` session's: that home is yours, kept, and its `.npmrc` or
gh login must survive):
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
