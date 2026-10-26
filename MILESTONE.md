# Milestones

Status: ✅ done · 🟡 partly done · ⬜ not started

| # | Milestone | Status |
|---|---|---|
| 0 | Feasibility spike: Firecracker on Apple Silicon | ✅ |
| 1 | `sbox setup` / `sbox doctor`, pinned downloads, guest image | ✅ |
| 2 | microVM runner: `sbox run` | ✅ |
| 3 | Network: per-run isolation ✅, fake internet services ⬜ | 🟡 |
| 4 | Guest instrumentation and skill entry points | ⬜ |
| 5 | Dependency prefetch | ⬜ |
| 6 | Analysis and report | ⬜ |
| 7 | Calibration on installed skills and plugins | ⬜ |
| v2 | Agent mode, snapshots, GitHub URLs | ⬜ |

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

### 3b. Fake internet services ⬜

Services inside each run's namespace that answer the guest's network
requests without anything leaving the machine, so a run can show which
destinations the skill tried to reach. To be scoped and reviewed as its own
step before implementation.

## 4. Guest instrumentation and skill entry points ⬜

Run a skill's automatic entry points inside the microVM: install hooks,
`hooks.json` / `settings.json` / `plugin.json` hook commands with realistic
event input, and stdio MCP server startup. Record process, file and network
activity with strace, streamed out of the guest as it happens.

## 5. Dependency prefetch ⬜

Get a skill's npm and PyPI dependencies into the run without running their
install scripts: a separate microVM with egress limited to the package
registries.

## 6. Analysis and report ⬜

Turn a run's recorded activity into findings with severities, a verdict and
JSON output, with the same exit codes as skills-audit (0 pass, 1 review,
2 block).

## 7. Calibration ⬜

Run sbox on the installed skills and plugins (the 53 targets from
`skills-audit -installed`), especially those with hooks and MCP servers, and
tune it until the noise is acceptable.

## v2 ⬜

- Agent mode: a headless agent using the skill inside the microVM. The API
  key is added by a proxy outside the guest and never enters it.
- Firecracker snapshots for sub-second starts.
- GitHub URLs as targets (v1: fetch locally first).

## Known limits

- Only code paths that actually run are observed. Code that waits, checks
  the date, or looks for a sandbox can stay quiet.
- The guest is arm64 only: x86 binaries in a skill won't run.
- Linux userland, so macOS-specific behaviour is only partly reproducible.
