# Live mount: the project folder itself, not a copy (new)

## Goal

`sbox dev` copies the project into the guest at boot, and changes come back
only through `sbox sync`. With a live mount the guest works on the Mac's
folder itself: an edit in the guest is on the Mac at once, and an edit on
the Mac is in the guest at once. There is no copy at boot and nothing to
sync.

Copy stays the default. Live is a mode a project asks for and its workspace
allows, or a workspace grants to every project in it:

```yaml
# ~/.config/sbox/config.yaml
workspaces:
  personal:
    paths: [~/workspace/]
    mount: live                     # granted: every project here is mounted live
  client-a:
    paths: [~/work/client-a/]
    allow: {live: true}             # a project here may ask for it

# ~/work/client-a/api/sbox.yaml
dev:
  mount: live                       # copy (the default) or live
  local: [node_modules, .next]      # kept on the guest's disk, not on the Mac
protect: [.env.production]          # read-only in the guest, added to the defaults
```

## Why

Copy mode keeps a review step between the agent and your files, but it
also means the editor on the Mac doesn't see the agent's work until a sync,
and the agent doesn't see the Mac's edits until the next boot. For a
project you trust and work on with the agent side by side, the folder
itself is what you want.

## What it gives up, and what stays

A live mount drops copy mode's promise that nothing reaches the Mac before
you've looked at it. What stays:

- **`.git` is read-only in the guest.** A guest that could write
  `.git/hooks` or `.git/config` (`core.fsmonitor`, `core.sshCommand`,
  `credential.helper`) would run code on the Mac at your next `git status`.
  The agent can read history, diff and branch names; it can't commit. You
  commit on the Mac.
- **`protect` paths are read-only too.** Defaults: `.git`, `.envrc`
  (direnv runs it), `.vscode`, `.idea`, `.husky`. The project's `protect`
  adds to them; nothing removes the defaults.
- **Read-only is enforced on the Mac**, by sbox's own file server, which
  checks every write against the protected paths by path. It holds against
  root in the guest too: the guest only ever speaks NFS to that server.
- **Nothing outside the project folder is reachable.** The server never
  follows a symlink on the Mac and stops `..` at the project root. The Lima
  VM keeps `--mount-none`.

What doesn't stay, and the docs say so: anything you run on the Mac in that
folder (`npm run`, `make`, the tests, an IDE task) runs what the agent
wrote. `protect` narrows that; it can't close it.

## What exists

- Copy mode: the project tar goes onto the session's work drive at
  `/home/runner/work` (`guest-init.sh`), the Mac's edits are pushed at boot
  (`pushOnBoot`), `sbox sync` merges both ways against a base, and `.git`
  never comes back.
- The Lima VM is created with `--mount-none` (`lima.go`); nothing in it can
  reach the Mac's files.
- Each session has a network namespace with a tap: the guest reaches
  `172.16.0.1` only, on the ports in the nft set `listening`
  (`sbox-run.sh`).
- The guest kernel (Firecracker CI's 6.1.155) has the NFSv4 client built
  in (4.0 to 4.2), and no NFSv3, 9p, FUSE or virtiofs; Firecracker has no
  shared-folder device either. The kernel mounts NFSv4 without `mount.nfs`.
- The guest already reaches the Mac through relays (`services`, ports),
  each a pair joined by a unix socket in the Lima VM.
- Workspaces decide what a project may reach (`workspace.go`), with grants,
  an `allow` ceiling, approve-once and BLOCKED lines.

## Design

### The path

```
guest /home/runner/work ──NFSv4.0──▶ 172.16.0.1:2049 (relay in the session's namespace)
   ──unix socket──▶ ssh -R over Lima's own ssh ──▶ unix socket on the Mac ──▶ sbox live-serve
```

- **The server is sbox itself**, a hidden `sbox live-serve <session>`
  started in the background by `sbox dev`, serving that session's project
  folder only, as you. It outlives the terminal, like the session, and
  exits when the session is gone (`sbox stop`, the exit prompt, or the
  session missing on a periodic check).
- **No TCP port opens on the Mac.** The server listens on a unix socket in
  sbox's state folder. An `ssh -R` over Lima's ssh (`limactl show-ssh`)
  carries it to the session's folder in the Lima VM, and a relay inside
  the session's namespace serves it on `172.16.0.1:2049` (2049 joins
  `listening`). Only that session's namespace has a path to it; another
  session, even one granted every service port, has none.
- **NFSv4.0**, the simplest version the guest kernel speaks: no sessions
  (4.1), no delegations, no pNFS. Byte-range locks are granted and tracked
  in memory, enough for one client.
- **File handles are the file's inode plus a MAC**, resolved through macOS's
  `/.vol/<fs>/<inode>`, with the path from `fsgetpath` for the protected
  check. Handles survive renames and a server restart (a reattach after the
  server died starts it again, and the guest's mount carries on); a forged
  handle fails the MAC.

### What the server refuses

- Following symlinks on the Mac: every path is walked one component at a
  time without following links. `READLINK` returns the link's text, which
  the guest resolves on its own side, where `/Users/...` doesn't exist.
- `..` past the project root.
- Any write to a protected path: `WRITE`, `SETATTR`, `OPEN` for writing,
  `CREATE`, `REMOVE`, `RENAME` from or to it, `LINK` to it. Creating a
  protected name that doesn't exist yet (a new `.envrc`) is a write too, and
  so is renaming a directory that contains a protected path. Names compare
  case- and normalisation-insensitively, as APFS does (`.ENVRC` is
  `.envrc`). Refusals are `NFS4ERR_ACCES` in the guest and BLOCKED lines in
  the summary and `sbox watch`.

### Ownership

The server acts as you, so the guest can do in the folder what you can, and
what it writes is yours on the Mac. It reports every file as owned by uid
1000, the guest's runner, so tools in the guest (git's ownership check
among them) see their own files.

### `local`: what stays in the guest

Dependency trees are tens of thousands of small files, slow over the
network path, and full of Linux binaries that don't belong in the Mac folder. Each `local`
path is a directory on the kept home, bound over the mount at that path:
`node_modules` in the guest is the guest's own, kept between sessions,
while the Mac's (if any) is untouched. Default: what copy mode already
leaves out (`node_modules`, `.venv`, `__pycache__`). The mount point itself
is an empty directory, created on the Mac if missing.

### File watching

The guest gets no inotify events for edits made on the Mac. A live session
sets `WATCHPACK_POLLING=true` and `CHOKIDAR_USEPOLLING=1` in `dev.env`'s
place (the project's `dev.env` wins), and `sbox config` says so.

### Git in the guest

`.git` is read-only, so the guest gets `GIT_OPTIONAL_LOCKS=0` (`git status`
doesn't try to write the index). `pushOnBoot` is skipped: there is no copy
to refresh. A guest-side overlay over `.git` that keeps the agent's commits
in the guest is left for later.

### Governance

- A project asks with `dev.mount: live`; it is allowed when its workspace
  has `allow.live: true` or grants `mount: live`, or with approve-once.
  Otherwise a BLOCKED line before boot:
  `BLOCKED mount   live  not allowed in workspace default → workspaces.default.allow.live: true`.
- `protect` from the project only adds paths. `dev.local` is the project's
  to set.
- GitHub targets stay copies (the cache folder isn't yours to edit live).
  `sbox run` and `sbox scan` stay copies.

### Sessions and the other commands

- The mode is part of the boot record: changing it needs `sbox dev
  -restart`, like `dev.vcpus`.
- `sbox sync` on a live session prints `live: the guest works on the
  folder itself, nothing to sync` and exits 0.
- The summary's `session 1h12m · sbox sync … shows what changed` becomes
  `session 1h12m · live, changes are on the Mac`.
- `sbox stop` stops the server, its `ssh -R` and the relay with the
  session.

## Spike results (2026-11-02)

On the real VM, with the Next.js app as the folder:

- **The guest kernel has NFSv4 only.** `/proc/config.gz`: `CONFIG_NFS_FS`,
  `CONFIG_NFS_V4`, `_V4_1`, `_V4_2` set; no `CONFIG_NFS_V3`, no FUSE, no 9p,
  no `NFSD`. A v3 mount fails with "Protocol not supported". A v4.1 mount
  works with the kernel alone, no `mount.nfs` helper:
  `mount -t nfs4 -o vers=4.1,addr=172.16.0.1,clientaddr=172.16.0.2 …`.
- **Re-exporting the Lima mount doesn't work.** Lima's virtiofs (vz) gives
  file handles that go stale once the inode leaves the cache:
  `open_by_handle_at` after `drop_caches` fails with ESTALE for every file
  and directory tried. `nfs-ganesha` (VFS) and kernel `nfsd` both serve by
  those handles. In practice one cache drop in the Lima VM made the whole
  guest mount stale, its root included; a nested read-only `.git` export
  was stale from the start. `unfs3` isn't packaged for Ubuntu 24.04.
- **Writes and ownership** worked while it lasted: the runner's writes
  landed on the Mac owned by you (uid squash). The guest saw every file as
  uid 0, which git would refuse as "dubious ownership" without
  `safe.directory`.
- **The guest reaches a server on the Mac's `127.0.0.1` today**, through
  the `services` relay (two relays and Lima's network): 638 µs per small
  round trip, measured over 2000 trips. No Lima mount is needed for that
  path.

So the server must track files by path, not by kernel handle, and it can
live on the Mac. That leaves two designs:

- **A. An NFSv4.0 server in Go, inside sbox, on the Mac.** The guest kernel
  as it is. The server runs as you (ownership is natural), on `127.0.0.1`,
  reached by the existing relay on `172.16.0.1:2049`; the Lima VM keeps
  `--mount-none`. File handles are opaque ids the server maps to paths, so
  protected paths are checked by path on every call and a crafted handle
  reaches nothing. Cost: NFSv4 is a large, stateful protocol (COMPOUND,
  client ids, open state, about 25 operations and the attribute encoding);
  a minimal 4.0 server without delegations or locks is roughly 2500 to
  3500 lines with tests. It is ours to test: Go table tests, then the real
  guest kernel as the client.
- **B. Our own guest kernel with 9p (or NFSv3, or FUSE) and a simpler
  server.** A 9P2000.L server over TCP is about half the code and
  path-based by nature, but sbox would build and pin its own kernel
  (Firecracker's config plus a few options) instead of Firecracker CI's,
  built at `sbox setup` (about 10 minutes) or published as a release
  asset. 9p also caches less than NFS, so it is slower per operation.

Recommendation: A. The cost is code we test like the rest; B moves it into
a kernel we would build, pin and update ourselves. **Chosen: A**
(2026-11-02); the design above is A.

## Code touchpoints

- `nfs.go` (new): XDR, RPC record marking, `COMPOUND` and the operations;
  handles, the no-follow walk, protected paths. `nfs_test.go` drives it
  with encoded compounds against a temp folder.
- `live.go` (new): `sbox live-serve`, its unix socket and `ssh -R`, start
  and stop from `sbox dev`, the periodic session check.
- `config.go`: `dev.mount`, `dev.local`, `protect`, validation, boot-record
  key.
- `workspace.go`: `mount` grant, `allow.live`, the request, BLOCKED line,
  approve-once.
- `scripts/sbox-run.sh`: `start --live`: the relay on `172.16.0.1:2049` to
  the session's socket, 2049 in `listening`, cleanup.
- `scripts/guest-init.sh`: the NFSv4 mount at `/home/runner/work`, `local`
  binds, the environment for watching and git.
- `sessions.go` / `sync.go`: no copy and no push at boot, sync's message,
  the summary line, `sbox config` output.
- `README.md`, `docs/config.md`, `MILESTONE.md`.

## Commits

- [x] **0. Spike, no product code.** Done 2026-11-02; results under
  "Spike results" above. The first design (re-export a Lima mount over
  NFS) doesn't work; A replaced it.
- [ ] **1. The server, read-only, and its path to the guest.** `nfs.go`:
  client id, `PUTROOTFH`/`PUTFH`/`GETFH`, `LOOKUP`/`LOOKUPP`, `GETATTR`,
  `ACCESS`, `READDIR`, `READLINK`, `OPEN` for reading, `READ`, `CLOSE`,
  `SAVEFH`/`RESTOREFH`, `RENEW`; handles; the no-follow walk. `live.go`:
  the socket, `ssh -R`, the relay. A VM test: the real guest kernel mounts
  it read-only, lists, reads, follows a symlink only on its own side.
- [ ] **2. Writes and protected paths.** `OPEN` for writing, `WRITE`,
  `COMMIT`, `CREATE`, `REMOVE`, `RENAME`, `LINK`, `SETATTR`, the locks;
  every protected-path rule above, case-insensitive. Table tests per rule;
  the VM test writes, and fails on each protected path as runner and root.
- [ ] **3. Config and workspace keys.** `dev.mount`, `dev.local`,
  `protect`, `mount` grant, `allow.live`, BLOCKED and approve-once,
  `sbox config` output.
- [ ] **4. Guest and host sides.** `guest-init.sh` mounts it and binds
  `local`, with the environment; `sbox dev` starts and stops the server, no
  copy or push at boot, `sync` and the summary, `-restart` on a mode
  change, the server restarted on reattach.
- [ ] **5. Docs, timings and the real VM.** README section with the
  trade-off in plain words, `docs/config.md`, `MILESTONE.md`; timings
  against copy mode on the Next.js app (`git status`, `npm ci` with
  `node_modules` local, `next dev` start and a reload after a Mac edit).

## Verification

- [ ] An edit in the guest is in the Mac's folder within a second, and an
  edit on the Mac is in the guest within a second; `next dev` reloads on a
  Mac edit with polling on.
- [ ] `touch .git/x`, `echo >> .git/config` and writing `.git/hooks/pre-commit`
  fail in the guest, as runner and as root; the same for `.envrc` and a
  project `protect` path. The Mac's `.git` is byte-identical afterwards.
- [ ] `npm ci` puts nothing in the Mac's `node_modules`; the guest's is
  there at the next session.
- [ ] Files written in the guest are owned by you on the Mac.
- [ ] A project in `default` asking for `live` gets the BLOCKED line and
  nothing boots; approve-once lets it start.
- [ ] A symlink in the folder to `~/.ssh` gives the guest nothing of the
  Mac's `~/.ssh`; `..` from the root stays at the root; a forged handle is
  refused.
- [ ] Another session granted every service port can't reach this
  session's server; no TCP port is listening on the Mac for it.
- [ ] Killing `sbox live-serve` and reattaching: the guest's mount carries
  on with the same files open.
- [ ] `sbox sync` on a live session says so; `sbox stop` leaves no server,
  `ssh -R` or relay behind.
- [ ] Copy-mode sessions behave exactly as before; the full VM suite
  passes.

## Considered and left out

- **Continuous sync instead of a mount** (`sbox sync -watch`): faster for
  dependency-heavy work and keeps inotify, but it is two copies kept close,
  not one folder.
- **Re-exporting a Lima mount over NFS** (`nfs-ganesha` or kernel `nfsd`
  in the Lima VM): the spike's first design; Lima's virtiofs handles go
  stale (see the spike results).
- **NFS straight from macOS's own `nfsd`**: it needs root changes to the
  Mac's `/etc/exports`, serves the whole Mac, and checks no protected
  paths.
- **Our own guest kernel with 9p, NFSv3 or FUSE** (design B): a simpler
  server, but a kernel to build, pin and update.
- **A TCP port on the Mac for the server**: reachable from any session
  granted that port; the `ssh -R` socket is reachable from one.
- **A writable `.git`**: commits from the guest would be convenient, and
  are exactly how a guest gets code to run on the Mac.

## Out of scope

- Live mounts for `sbox run`, `sbox scan` and GitHub targets.
- Keeping the agent's commits in a guest-side overlay over `.git`.
