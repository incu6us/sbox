# Firecracker snapshots

## Status: built for sessions, a small gain

A session on a project resumes a snapshot instead of booting. One-shot `sbox
run` and `sbox scan` still boot. Measured on an M4 Pro, on a small project,
after the other start-up work (`MILESTONE.md`):

| | Cold boot | Restored |
|---|---|---|
| Firecracker and kernel, or the load, until `guest-init.sh` runs | ~1.2 s | ~0.5 s |
| `guest-init.sh` until `sshd` is up | 0.74 s | 1.5 s |
| `sbox-run start` in total | 2.26 s | 2.0 s |

The gain is about 0.25 s, far less than the kernel's time suggests. A restored
guest takes a fault on every page it first touches, through two layers of
virtualization, so everything after the restore runs slower. Firecracker's
answer to that (huge pages with a userfaultfd handler) isn't worth building
for it. `SBOX_SNAPSHOT=0` in the VM-side script's environment turns restoring
off.

## Design

- **What is snapshotted.** `sbox-run snapshot <vcpus> <MiB>` boots a guest on
  a placeholder run drive and an empty work drive. Stage-one init runs the
  script the drive carries, as always; that script is the stub in
  `sbox-run.sh`. It reruns itself from memory (a script on the drive would
  keep it mounted), runs what a boot runs to warm the page cache, unmounts the
  drive, prints `SBOX-SNAP READY` and loops trying to mount `/dev/vdb` until
  the drive holds `sbox/nonce`. sbox-run pauses it there and takes the
  snapshot. **It holds no session's anything:** no project, nonce, key or
  secret has been near that guest. The image didn't have to change.
- **Waking up.** Restoring needs no signal: the same jail paths now hold the
  session's drives, so the loop's next mount finds the real one and execs its
  `init.sh`, 30 ms apart at most.
- **What must match.** Firecracker restores the device set it saved: the
  same drive ids and paths, tap name and MAC, vCPUs and memory. So there is
  one snapshot per size, in `$state/snap/<vcpus>-<MiB>`, the newest three
  kept. The guest remembers drive sizes, so a restored session's run drive
  is always 1024 MiB (sparse); one that wouldn't fit boots. The work drive
  is always 20 GiB.
- **What must not match.** The guest wakes with the snapshot's clock and
  random state. `sbox-run` puts the time and 64 fresh random bytes on the
  drive, root-only; `guest-init.sh` sets the clock, writes the bytes to
  `/dev/urandom` and forces a reseed (`RNDRESEEDCRNG`) before anything else.
- **Staleness.** A stamp (inode, size and time of the kernel, image and
  Firecracker, and the stub's text) must match, or the session boots and the
  snapshot is taken again. A snapshot Firecracker refuses is deleted and the
  session boots.
- **When it is taken.** A session with no snapshot of its size boots, and 20 s
  later a background process takes one, so the next session resumes. The
  memory file is copied sparse: about 65 MB for the default 1 GiB.

## Verification

- [x] `TestSnapshotRestoreOnVM`: the console shows the load, the guest's clock
  is the Mac's, two restored guests draw different random bytes, each has its
  own project, a writable home and its network.
- [x] Unit tests with fakes: the snapshot's API calls, one per size, no jail
  or namespace left; a restored session has no boot config and the drive at
  1024 MiB with the time and seed on it; a refused load falls back to a boot
  and deletes the snapshot; another size boots and gets its own; a rebuilt
  image boots.

## Out of scope

- Snapshots for one-shot `sbox run` and `sbox scan` (their device set varies).
- Warm pools of restored guests.
- Huge pages with a userfaultfd handler, the way past the page-fault cost.
