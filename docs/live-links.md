# Live links: the project, other folders and files, read-only or read-write (new)

## Goal

`dev.mount: live` links the project folder, read-write only. `mounts` still copy
everything else in at boot, and nothing comes back. Let the project be linked
read-only too, and let a mount be a link: the guest works on the Mac's own folder
or file, read-only or read-write, with nothing to sync.

```yaml
dev:
  mount: live-ro                                                               # the project, read-only; live is read-write
mounts:
  - {host: ~/work/shared, guest: /home/runner/shared, live: true}              # read-only, the default
  - {host: ~/work/out,    guest: /home/runner/out,    live: true, mode: rw}    # read-write
  - {host: ~/.config/app/rules.yaml, guest: /home/runner/rules.yaml, live: true}
```

Copies stay the default, and `sbox run` and `sbox scan` stay copies.

## Why

`sbox sync -apply` is a manual step between the agent and your files. For the
project, `dev.mount: live` already removes it. What's left is everything
outside the project: a shared library the agent should see at its current
state, or an output folder it should fill. Today those are stale copies, or in
`sbox dev` they don't come back at all.

## Where it is configured

The same rule as every key that reaches past the VM: a project asks, your own
file grants. Workspaces exist only in `~/.config/sbox/config.yaml`, since a
downloaded repository must not be able to grant itself anything. What belongs
to one project goes in its `sbox.yaml`:

```yaml
# ~/work/client-a/api/sbox.yaml: the project asks
dev: {mount: live}
hide: [.env]
mounts:
  - {host: ~/work/client-a/shared, guest: /home/runner/shared, live: true}
  - {host: ~/work/client-a/out,    guest: /home/runner/out,    live: true, mode: rw}

# ~/.config/sbox/config.yaml: your file allows, for the projects in a folder
workspaces:
  client-a:
    paths: [~/work/client-a]
    allow:
      live: true                       # dev.mount: live, and any read-write link
      mounts: [~/work/client-a/shared]
      rw: [~/work/client-a/out]
```

Without the `allow`, `sbox dev` prints the BLOCKED lines and, in a terminal,
offers to allow them for this project alone and remembers the answer. So a
single project needs no edit to your own file. A workspace's own `mounts` are
grants and take `live` as well.

| Link | The project asks | Allowed by |
|---|---|---|
| The project, read-only | `dev.mount: live-ro` | `allow.live`, a workspace's `mount: live` or `mount: live-ro`, or approve-once |
| The project, read-write | `dev.mount: live` | `allow.live`, a workspace's `mount: live`, or approve-once |
| Read-only mount | `live: true` | `allow.mounts`, or approve-once |
| Read-write mount | `live: true, mode: rw` | `allow.rw` and `allow.live`, or approve-once |

A read-only project still needs permission: the guest reads `.gitignore`d files
that copy mode leaves out. Approving `live-ro` doesn't approve `live`, and a
project under a workspace's `mount: live-ro` that asks for `live` gets a BLOCKED
line.

`allow.rw` alone isn't enough, because it was written for `sbox run -apply`,
which shows the changes and asks first. A link has no such step, so a config
written before this key must not start granting it. For the same reason the
request's text differs (`(live, rw)`), and an older approval doesn't carry over.

## What it gives up, and what stays

A read-write folder link drops the review step, as `dev.mount: live` does. An
`rm -rf` in the guest deletes on the Mac, and there is no undo. A file link never
passes on a deletion (see Single files).

Every rule of the live server holds in a link, from the link's own root: no
symlink followed on the Mac, nothing outside the root, `.git`, `.envrc`,
`.vscode`, `.idea`, `.husky` and files named `HEAD` read-only at any depth,
and `hide` and `protect` applied. An anchored `hide` or `protect` rule is
matched from each link's root as well as the project's; hiding or protecting
too much is the safe direction, and it needs no per-link keys.

### Read-write can run code on the Mac

Those names protect a project. Outside one, plain files run code: a guest that
can write `~/.gitconfig` sets `core.fsmonitor`, and it runs at your next git
command in any repository. So a read-write link is refused, whoever asks and
whatever is allowed, when its resolved Mac path is, holds or is inside:

- the home folder itself, or anything above it
- `.ssh`, `.gnupg`, `.aws`, `.config`, `.claude`, `$CLAUDE_CONFIG_DIR`, `Library`
- `.gitconfig`, `.zshrc`, `.zshenv`, `.zprofile`, `.zlogin`, `.bashrc`,
  `.bash_profile`, `.profile`, `.npmrc`
- any folder in the Mac's `$PATH`

Each name is compared after resolving its own symlink, so `~/dotfiles` holding
the real `.zshrc` is refused too, and compared as APFS does (`.ZSHRC`). There's
no key to turn this off, by decision: a prompt approved in a hurry is how a bad
project would get past it. It has false positives (`~/.config/foo/out`, or `~/work`
with `~/work/tools/bin` in `$PATH`); if real ones show up, the answer is a key only
your own file can set, never a prompt. The list can't be complete, and the docs say
so: a read-write link is for data, not for anything a tool on the Mac reads as
configuration or runs.

The list applies to `mounts` entries with `live: true` and `mode: rw`, and nothing
else: not the project folder, read-only links, copied mounts, `sbox sync` or `sbox
run -apply`. A read-only `~/.ssh` exposes no more than today's copy does.

## What exists

- `nfs_darwin.go`: one `nfsServer` per root. `protected()` is the one check
  behind every write, and `hidden()` the one behind every look-up. Handles are
  a device and inode with a MAC.
- `live_darwin.go`: `sbox live-serve <project> <dir>`, one process per project,
  with key, token, port, pid and log in `live/<project>/`. `liveAccept` closes a
  connection without the token.
- `sbox-run.sh`: `fwd_pair ... live <port>` hard-codes the guest port 2049 and
  the single `$run/live.token`; 2049 joins the nft set `listening`.
- `guest-init.sh`: mounts `172.16.0.1:/` over `~/work`, waiting up to 30 s.
  `sbox/mounts` already shows the pattern for a guest path on the kept home
  (made by the runner, `realpath` checked).
- `config.go`: `mount{Host, Guest, Mode}`, and `mountSources` resolves hosts.
  The boot key `mounts` is applied to a running session by `copyMounts`.
- `workspace.go`: `allowsMount`, `request`, BLOCKED lines, approve-once.
- `sbox clean` removes all of `live/`.

## Design

### The path

```
guest ~/out ──NFSv4.0──▶ 172.16.0.1:<20490+n> (relay in the session's namespace)
   ──unix socket──▶ expose (Lima VM, sends this link's token) ──▶ the Mac's 127.0.0.1:<port>
   ──▶ sbox live-serve -link ... (its own root, key, token, port)
```

The same path as the project's mount, once per link. A link costs no new
protocol code and no shared state between roots, and one link's lock doesn't
hold up another's calls.

- **One process per link.** `sbox live-serve -link <id> [-ro] [-file <name>]
  -guest <path> <project> <dir>`, with its state in
  `live/<project>/links/<id>/`. `<id>` is a hash of the resolved path, mode and
  kind, and `live-serve` checks it against its arguments. A changed mode is a
  different server, so a running server's mode never changes. It stops as the
  project's server does, once the project has no session. ponytail: a `ps` into
  the VM per link every 30 s; one process with many listeners if that shows up
  in `limactl` load.
- **At most 10 links, guest ports 20490 to 20499**, by position in the list, so
  there's no allocator. `services` and raw `egress` in that range are refused by
  `validate`, as is an eleventh link. ponytail: a fixed range, an allocator if
  anyone needs an eleventh.
- **Tokens stay off the guest's drive**, as `live.token` does: `links/<port>.token`
  on the run's drive, moved root-only into the run folder by `sbox-run.sh`
  before the guest's drive is built.
- **`sbox/links`** tells both sides what to do, one line per link, tab-separated:
  id, guest port, Mac port, `ro|rw`, `dir|file`, the file's name or `-`, guest path.
  `validate` refuses control characters in the guest path and the file's name.
  `sbox-run.sh` checks both ports with a regex, as it does `sbox/live`.

### Read-only

`nfsServer.readOnly`, set by `-ro`: `protected()` returns true for every path,
the root included. Everything that refuses a protected path then refuses, on
the Mac, whatever the guest's user. The guest mounts it without the `ro` option:
a write reaches the server, is refused like `.git`, and lands as a `BLOCKED live`
line. With `ro` the guest's kernel would stop it first and you'd never hear of it.

### The project, read-only

`dev.mount: live-ro` is one more value of the key, so the boot record and
`-restart` on a change already work. `live()` is true for both; a new `liveRO()`
sets `-ro`. A workspace's `mount` takes it as well.

- **Each mode has its own server.** Today a running server is reused, so one left
  from a read-write session could serve a read-only one. `live-ro` keeps its state
  in `live/<project>/ro/`, with its own port and token. The old server is
  unreachable with its relay gone, and stops as usual. `protect`, `hide` and
  `refused` stay at the project's level.
- **`dev.local`.** A mount point can't be made from the guest here. Paths the project
  lists are made on the Mac by `sbox dev` before boot, walked without following a
  symlink, and one that goes through a symlink stops with an error: `sbox.yaml` is
  the project's, and it mustn't place a folder elsewhere. The defaults are bound only
  where they already exist, so a read-only session leaves nothing on the Mac.
- **What breaks.** Anything writing inside the project: builds, `dev.setup`, an
  `npm ci` without `node_modules` in `dev.local`. Each is a `Permission denied` in
  the guest and a BLOCKED line in the summary.
- **The summary** says `live, read-only`, since nothing came back. `sbox sync` says
  there's nothing to sync, as for `live`.

### Guest paths

A link may not be at, inside or around what sbox writes in the home after the
live mount, or that write would land on the Mac: `.ssh` (`guest-init.sh` removes
and links `environment` there), `.claude`, `.claude.json`, `.sbox`, `.npm`,
`.cache`, and `work`. The fake credentials (`.aws`, `.config/gh`, ...) are
written before any mount and sit underneath, so a link over one is fine, as a
copy is today.

`validate` also refuses links that nest in a copied mount or in each other, in
either direction. `copyMounts` and `sbox/mounts` both run `rm -rf` on their
guest path, and with a read-write link around it that deletes on the Mac.

In `guest-init.sh` the links are mounted last, after the `sbox/mounts` copies,
so nothing of sbox's writes through one. Parents are made by the runner. A folder
link's guest path must equal its own `realpath -m`, since a link the runner left on
the kept home would otherwise steer root's mount. A failed mount prints one line,
as the project's does, and the session carries on; all links share one 30 s wait.

### The project folder and a link

A link that holds the project, or is inside it, is refused by resolved path.
`{host: ~/work}` around a project at `~/work/app` would show `~/work/app/.env`
past the project's `hide`, and write past copy mode's review.

### Single files

An NFS root is a folder, and that decides the rest. Linux also refuses a rename
over a mount point with `EBUSY`, and many tools save by rename, so a file link is
never bound over its guest path.

- **The export is the file's parent, with only that name visible.**
  `nfsServer.only` makes `hidden()` true for anything else at the root, so
  `LOOKUP`, `READDIR`, stale handles and refused changes all come from the one
  existing check, and a sibling can't be read, made or moved into view. The root
  is resolved first, so a dotfile linked from a dotfiles repository exports the
  repository's folder with the one name. A name `hide` covers is refused by
  `validate`, as it would be missing in the guest.
- **The guest mounts it out of the way**, at `/run/sbox/links/<port>`. A read-only
  link takes the default options: a rename on the Mac showed within 33 ms, with
  `ESTALE` in 1 round of 80. A read-write link's mount adds `actimeo=0`, so the
  loop's `lstat` sees a change at once; `ESTALE` is common there (see the spike), and
  the loop only retries at its next check.
- **Before either kind**, the parent of the guest path must equal its own `realpath
  -m`. A folder at the guest path prints a line and is skipped, never removed.

**Read-only: a symlink.** The guest path links to `/run/sbox/links/<port>/<name>`,
made by the runner, replacing a link or file left there. The guest looks the name up
each time, so a rename on the Mac simply shows. If the guest replaces the link, only
its own view changes.

**Read-write: an ordinary file, and a copy loop.** The guest path is a regular file
on the kept home, so every tool's way of saving works. `scripts/sbox-links.py` (with
`--selftest`) keeps it and the hidden mount's file the same. It starts only when a
session has a read-write file link, and runs as the runner, so it can do nothing the
guest can't already do, and the server checks every write. A root loop in
`guest-init.sh` restarts it if the agent kills it.

Every 0.5 s it compares each side's `lstat`:

- **A change is copied once its `lstat` has held for one check**, so a half-written
  file isn't sent.
- **The direction comes from a hash of what both sides last agreed on**, kept on the
  kept home at `~/.sbox/links/<id>`. Only the guest changed: the guest's goes to the
  Mac. Only the Mac changed: the Mac's comes in. Both changed, differently: the Mac
  wins, and the guest's version is saved to `~/.sbox/conflicts/<name>.<time>` with a
  line on the console.
- **Toward the guest** it writes a temp file beside the path and renames, with the
  Mac's mode bits. **Toward the Mac** it writes in place, then truncates to the new
  length, so the Mac's inode, mode and any symlink to it stay. Truncating first left an
  empty file a reader on the Mac could catch, and a stop at that moment would lose it.
  ponytail: a reader on the Mac can still catch a few ms of mixed content; a server-side
  temp name if that ever bites.
- **It only reads regular files, with `O_NOFOLLOW`.** Anything else at the guest path
  prints one line and the file is left alone until it's regular again.
- **A deletion isn't passed on, either way.** While a side is missing the loop waits,
  so a tool that unlinks before it writes can't take the Mac's file with it. A change
  of mode bits in the guest isn't passed on either.
- **At most 16 MiB.** A larger file on either side prints one line and isn't copied.
  ponytail: a whole-file copy and a hash; link the folder for a database or a large log.
- **At boot** the same rules apply. So a change the last check didn't reach before a
  hard stop goes to the Mac at the next boot. With no record yet and a different file
  already at the guest path, it counts as both changed.

A read-write file link isn't strictly one file: it's two kept within about a second.
`sync -watch` stays rejected for folders, where the cost grows with the tree; one
small file doesn't have that problem. A folder link has none of this, since a tool's
temp file and rename both happen inside the mount.

### Sessions and the other commands

- **Boot keys.** Live mounts get their own key, `mounts.live`, set only when there
  are any. `mounts` stays the copies alone, with the same JSON as today, so
  existing sessions still match and `applyLive` keeps working unchanged. `live`
  is `json:"-"`. Changing a link needs `sbox dev -restart`, like `dev.mount`.
- **`job.mounts` leaves live mounts out**, at boot and at attach. Otherwise
  `copyMounts`' `rm -rf` would run on a link's guest path.
- **Reattach** starts each link's server again if it stopped, on its port and
  key, so the guest's mount carries on, as the project's does.
- **Refusals** go to the project's one `refused` file, prefixed with the guest
  path (`/home/runner/out/.git/x`), so the summary and `sbox watch` need no change
  beyond reading them.
- **`sbox config`** prints a line per link. **`sbox sync`** is unchanged: it carries
  the project, and a link was never in it.
- **`sbox run`** treats `live: true` as a copy, `mode: rw` as today's `-apply`.
- **`-fresh` and `sbox clean`** work on the stopped drive, so they can't reach
  through a link.

## Spike results (2026-11-05)

On the real VM, with a throwaway test and `guest-init.sh` patch, and services standing
in for the relays:

- **Several mounts at one IP work.** Three NFSv4.0 mounts on three ports: three rows in
  `/proc/fs/nfsfs/servers`, one connection per server, each root its own, the callback
  socket on 1023 shared. The design stands.
- **A rename-over on the Mac, through a symlink.** With default options: `ESTALE` in 1
  round of 80, the new content within 29 ms. With `actimeo=0`: `ESTALE` in 57 rounds of
  80 (the client asks about the old handle first), within 33 ms. So read-only links
  take the defaults. An edit in place on the Mac showed within 26 ms. A descriptor held
  open across a rename gets `ESTALE`, as in any live mount.
- **The copy loop.** Each save reached the Mac with no `EBUSY` and left a regular file:
  `echo >` 1.08 s, `sed -i` 1.09 s, `git config --file` 1.02 s, `os.replace` 1.03 s, a
  temp file and rename in Node 0.77 s after 0.35 s of running, in place 1.01 s. Toward
  the guest: 1.0 s for a rename on the Mac, 1.02 s for an edit in place. Idle, it used 5
  CPU ticks of 3000 in 30 s (0.17% of a CPU). A writer adding a line every 0.1 s for 4 s
  reached the Mac once, whole.
- **Claude Code's own edit tool wasn't run**: it needs a model call. A regular file on
  ext4 can't give `EBUSY`, and Node's temp file and rename stood in for it. Commit 9's VM
  test runs it if a token is there, and says so if not.
- **Boot**: 1.05 to 1.11 s with no mounts, 1.09 to 1.10 s with two.

## Code touchpoints

- `nfs_darwin.go`: `readOnly`, `only`.
- `live_darwin.go`, `live_other.go`: `liveState` takes its folder; `live-serve -ro`
  and `-link`; `startLink`; the guest path on refusals.
- `config.go`: `live-ro`, `liveRO()`, `mount.Live`, `validate`, `bootKeys`, the guest
  path and port rules, the read-write refusals.
- `workspace.go`: `mount: live-ro`, the project's gate, `allowsMount`'s caller, the
  request text.
- `sessions.go`, `run.go`: the server by mode, read-only `dev.local`, leave live mounts
  out of `job.mounts`, `job.links`, the `sbox/links` and token files, reattach, the
  project overlap check.
- `diag.go`: the summary, the `sbox config` lines.
- `scripts/sbox-run.sh`: `fwd_pair`'s `link` kind, the tokens, `listening`.
- `scripts/guest-init.sh`: the mounts, the symlinks, starting the loop, skipping a
  default `dev.local` that isn't there.
- `scripts/sbox-links.py` (new): the copy loop, with `--selftest`, embedded in `run.go`.
- `sessions_vm_test.go`: the VM tests.
- `README.md`, `docs/config.md`, `docs/README.md`, `MILESTONE.md`.

## Commits

Strict TDD, each behaviour's test first. On a branch: no merge before commit 4,
as a link is ungated until then.

- [x] **0. Spike, no product code.** Done 2026-11-05; results above.
- [x] **1. Read-only server.** `readOnly`. A table test: every changing operation
  (`OPEN` for writing or creating, `WRITE`, `SETATTR`, `CREATE`, `REMOVE`,
  `RENAME`, `LINK`) refused at the root and at depth, `ACCESS` read-only, reads
  and `READDIR` unchanged.
- [ ] **2. The project, read-only.** `dev.mount: live-ro`, its gate and request text,
  a test that approving `live-ro` doesn't approve `live`. A server per mode, with a
  test that a running read-write server isn't reused. `dev.local` made on the Mac,
  refusing a symlink on the way; defaults only where they exist. The summary and
  `sbox config`. VM test: writes refused as the runner and from the raw client,
  `dev.local` works, the Mac's files byte-identical.
- [ ] **3. The config key, and no copy of a link.** `mount.Live`, `mounts.live`
  (a test that a recorded session without it still matches), the guest path rules,
  nesting, the port range and the cap. A test that `job.mounts` and `copyMounts` never
  see a live mount; `sbox run` copies.
- [ ] **4. Gating and refusals.** `allow.mounts` for read-only; `allow.rw` and
  `allow.live` for read-write; the request text and a test that an old approval
  doesn't carry; BLOCKED lines. The read-write refusals as a table, with symlinks,
  case, `$CLAUDE_CONFIG_DIR` and `$PATH`, and that approve-once can't pass them.
  The project overlap check, both directions.
- [ ] **5. A server per link.** `liveState` by folder, `live-serve -link`, `startLink`,
  an id that doesn't match refused, `job.links`, `sbox/links` and the token files
  written. Tests with a fake `startLive`.
- [ ] **6. The path in the VM: folders.** `fwd_pair`'s `link` kind, tokens moved off
  the drive, `listening`, tested from Go with fake commands (bash 3.2 and 5).
  `guest-init.sh`'s mounts. VM test: a read-only and a read-write folder in a
  copy-mode session and in a live one.
- [ ] **7. Lifecycle.** Reattach restarts servers, `-restart` on a change,
  `BLOCKED live` lines with the guest path, the `sbox config` line, and stop and
  clean leave nothing behind. VM test: kill a link's server, reattach, files
  still open.
- [ ] **8. Read-only files.** `only`, with table tests; `validate`; the mount and the
  symlink. VM test: a rename-over from the Mac shows, no sibling visible, and a guest
  replacing the link leaves the Mac untouched.
- [ ] **9. Read-write files.** `sbox-links.py` with its `--selftest` (each direction,
  conflict, deletion, half-written, a symlink at the path, the cap, boot after a hard
  stop), the root loop, `validate` on protected names. VM test recording what each
  tool does.
- [ ] **10. Docs and timings.** README section with the trade-off in plain words,
  `docs/config.md`, `MILESTONE.md`, boot times with and without links.

## Verification

- [ ] An edit in the guest is on the Mac within a second, and the reverse within
  seconds, for a read-write folder.
- [ ] In a read-only folder or file, writes fail as the runner, and from the raw
  NFS client as uid 0; the Mac's files are byte-identical; refusals are BLOCKED
  lines.
- [ ] A read-write link keeps `.git`, `.envrc`, `HEAD`, `hide` and the symlink rules.
- [ ] A file link shows no sibling, by `ls`, `LOOKUP` or a forged handle, and can't
  make one.
- [ ] A rename-over on the Mac reaches the guest, read-only and read-write.
- [ ] In a read-write file link, `echo >`, `sed -i`, `git config --file`, `os.replace`
  and Claude Code's edit tool all reach the Mac within about a second, with no `EBUSY`.
- [ ] Both sides changed at once: the Mac's version stays, and the guest's is in
  `~/.sbox/conflicts/`. `rm` on either side deletes nothing on the other.
- [ ] A symlink to a root-only file at a read-write file link's guest path sends
  nothing to the Mac.
- [ ] A change just before a hard stop is on the Mac after the next boot.
- [ ] `dev.mount: live-ro`: writes refused as the runner and from the raw client as
  uid 0; `-restart` from `live` leaves no way to write through the old server; a
  `dev.local` through a symlink stops before boot.
- [ ] One link's token and port get nothing from another link or session.
- [ ] `~/.gitconfig`, `~/.zshrc`, `~`, `~/.config/x` and a `$PATH` folder as read-write
  links are refused, approved or not, directly and through a symlink.
- [ ] A symlink the runner left at a link's guest path on the kept home stops that
  mount, and nothing is mounted where it pointed.
- [ ] Attaching with a changed copy mount doesn't touch a link's files.
- [ ] `-fresh`, `sbox clean` and `sbox stop` delete nothing on the Mac and leave no
  server, relay or token behind.
- [ ] Boot time with no links is unchanged; sessions without `live` behave as before;
  the full VM suite passes.

## Known limits

- A read-write file link lags by up to about a second, and when both sides change
  within that the Mac wins, with the guest's version saved. A change not yet copied at
  a hard stop is lost with `-fresh`.
- A read-write file link is capped at 16 MiB, shares no locks with the Mac (no SQLite),
  and passes on neither deletion nor mode bits. Link the folder for those.
- A read-only file link is a symlink, so a tool that checks with `lstat` sees one, and
  a read racing a rename on the Mac can rarely get `ESTALE` (1 in 80), as can a
  descriptor held open across one.
- A link gets no file events for the Mac's edits, and `WATCHPACK_POLLING` is set
  only for a live project.
- A link is NFS: a dependency tree in one is slow, and `dev.local` covers the project
  only.
- The read-write refusals are a list; it narrows the risk and can't close it, and it
  refuses some harmless paths.
- A server for a removed link, or for a project's earlier mode, keeps running,
  unreachable with its relay gone, until the project has no session.

## Considered and left out

- **`sbox sync -watch` for folders**: two copies kept close, not one folder, with a
  cost that grows with the tree. The loop is this idea for one small file only.
- **Binding a file over its guest path**: a rename in the guest fails with `EBUSY`, and
  a rename on the Mac needs the server to hand out handles by name, with a fixed
  `fileid` and `NFS4ERR_DELAY` for the gap.
- **A symlink for read-write files**: a rename in the guest silently ends the link, and
  a tool that resolves it can't make its temp file beside the real one.
- **Mounting a file's parent folder, with temp files in a scratch area**: `~/x` is
  impossible, since the parent would be the home, and the server would need files
  outside its root.
- **One server, one port, a path per link (a pseudo-root)**: synthetic folders and
  `fsid` changes in the server, for what separate ports get for nothing. It's the
  fallback if the spike's first check fails.
- **A per-link `protect` or `hide`**: `mode` and the shared lists cover it until a
  project needs otherwise.
- **`dev: {mount: live, mode: ro}`**: it would match `mounts`, but needs a new key, a
  merge rule and a workspace field, for what one more value of `dev.mount` gets. Their
  defaults differ anyway: a live project is read-write, a link read-only.
- **A switch for the read-write refusals**: the point is that no approval passes them.

## Out of scope

- Links for `sbox run` and `sbox scan`.
- Polling variables and `dev.local` for links.
