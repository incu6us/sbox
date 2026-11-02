# Firecracker snapshots for sub-second starts (v2)

## Status: measure first

A run costs about 1.4 s end to end and a two-hook scan 6.5 s, most of it
not the boot. Before building anything, commit 0 is a measurement: where
the time goes in one `sbox run` and one `sbox scan`, from `limactl shell`
startup, tar streaming, `mkfs.ext4`, jailer setup, kernel boot, guest init
(tmpfs, credentials, copy of the skill), to the job itself and the
poweroff. If the boot and guest init together are under a third of a scan,
this item is closed as not worth its complexity, with the numbers in
`MILESTONE.md`. The plan below is for the other outcome.

## What exists

- `sbox-run.sh: boot` writes `vm.json` and starts Firecracker with
  `--no-api --config-file`; one microVM per call, 2 vCPU, 1 GiB.
- The phase comes from the kernel command line (`sbox.phase=prefetch`),
  read by `guest-init.sh`. The run's drive is attached at boot, read-only.
- Kernel and rootfs are hard-linked into each jail; drives are per run.

## Design

**What is snapshotted.** One snapshot per image revision, taken by `sbox
setup` after provisioning: a microVM booted from the shared rootfs with a
placeholder run drive, stopped in the stub init (`/sbin/sbox-init` in the
image) at the point just before it mounts `/dev/vdb`. The stub prints
`SBOX READY` and waits for a line on the serial console. Memory (`mem`,
sparse, up to 1 GiB) and `vmstate` go to `/opt/sbox/snap/`, keyed by the
rootfs stamp and the Firecracker and kernel sums, and are deleted whenever
any of those change.

**What must match on restore.** Firecracker restores a VM against the same
device set: same drive ids and paths inside the jail, same tap name, same
MAC, same vCPU and memory sizes. So:

- The run drive is always the same size (a 512 MiB sparse file, since the
  tar is capped at 256 MiB and the ext4 overhead is small), at `/drive.ext4`
  in the jail. The guest never read the placeholder, so no stale page cache;
  the mount after restore reads the real drive.
- The dependency drive is attached at snapshot time too, as an empty
  placeholder of fixed size (4 GiB sparse already), so the main phase with
  prefetched dependencies restores against the same layout. Without
  prefetch, the placeholder stays attached and empty, and the init sees no
  wheels or caches, as today when `/dev/vdc` is absent (the check becomes
  "mountable and non-empty").
- `sbox.phase` moves from the kernel command line to a file on the run
  drive, `sbox/phase`, since the command line is frozen in the snapshot.

**Restore flow in `boot`.** The jailer starts Firecracker with an API socket
instead of `--no-api`; `sbox-run` sends `PUT /snapshot/load` with the
snapshot and memory paths (hard-linked into the jail) and `resume_vm:
true`, then writes one line to the serial console to release the stub,
which mounts the drive and execs `init.sh` as today. `curl --unix-socket`
is in the Lima VM.

**After restore, inside the guest.** The RNG state is cloned from the
snapshot, so two restored guests would draw the same random numbers. The
guest init writes the nonce and canary (host randomness, per run) into
`/dev/urandom` before anything else; that mixes the pool without needing an
entropy credit, and nothing in the guest depends on `/dev/random` blocking.
The clock jumps: the init sets the date from a `sbox/now` file on the
drive, written by `sbox-run`, so timestamps in traces make sense. Network
configuration runs after restore as now (`eth0` is up, the address isn't
set yet in the snapshot).

**The prefetch VM** keeps a cold boot: it is one per scan at most, writes
to its drive, and the 1 s saved isn't worth a second snapshot flavour.

## Commits

- [x] **0. Measure.** A `-timing` flag on `sbox run` printing phase timestamps
    from markers the guest already prints plus a few new ones (`SBOX-$nonce
    T <phase>`). Numbers into the milestone; decision made here.
    - Measured without the flag, from the guest's `/proc/uptime` at the
      first attach (2026-10-28, M4 Pro): a session start is 2.4 s, of
      which kernel boot and guest init are about 1.8 s and the host side
      (`limactl shell` 0.05 s, tar, `mkfs.ext4`, keys, jailer) about
      0.6 s; attach 0.5 s, stop 0.4 s.
    - Decision: deferred. Boot is three quarters of a start, so a snapshot
      would roughly halve it, but in dev mode a start happens once per
      working session and coming back is a 0.5 s attach; restoring
      per-session drives, nonce and network into a snapshot isn't worth
      1.8 s once a session. Revisit if one-shot `run` or `scan` speed
      matters again.
- [ ] **1. Fixed drive sizes and `sbox/phase` on the drive.** No snapshot yet;
    verifies nothing else depends on the drive size or the command line.
- [ ] **2. Snapshot at setup.** The `READY` stub, `sbox setup` takes the
    snapshot through the API, doctor checks it exists and matches the stamp.
- [ ] **3. Restore in `boot`.** Behind an environment variable on the VM side
    first (`SBOX_SNAPSHOT=1`), then default when the probes pass; the cold
    path stays as the fallback when the snapshot is missing.
- [ ] **4. Reseed, clock, verify, document.** Timing before and after in
    `MILESTONE.md`.

## Verification

- [ ] Two concurrent restored runs with different nonces: each reports its
  own, markers don't cross (same addresses in separate namespaces, as the
  3a test).
- [ ] A run after a rootfs rebuild uses a fresh snapshot (stamp mismatch
  deletes the old one).
- [ ] Doctor's run probe, the network boundary, DNS logging, prefetch and
  egress all pass unchanged.
- [ ] `sbox run <dir> -- date` is within a second of the host; two runs print
  different `od /dev/urandom` output.
- [ ] Jail cleanup still leaves nothing; snapshot files are the only new
  persistent state and live under `/opt/sbox`.

## Out of scope

- Snapshotting after the skill is copied in (the skill is per run).
- Warm pools of restored VMs waiting for jobs.
- Snapshots for the prefetch phase.
