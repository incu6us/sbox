# sbox

Runs AI agent skills inside a Firecracker microVM with fake secrets and a
fake internet, and reports what they try to do. Work in progress: see
[MILESTONE.md](MILESTONE.md) for what's done and what's next.

```
macOS ── limactl ──▶ Lima VM "sbox" (vz, nested virtualization, no host mounts)
                     └─ Firecracker ──▶ microVM per run (node, python, uv, strace)
```

```sh
go build -o sbox .
./sbox setup    # create + provision the VM, then run doctor (first run ~1 min)
./sbox doctor   # ~7 s: VM, KVM, pinned artifacts, microVM boot, network boundary
./sbox run ./some-skill -- node --version    # ~1.4 s per run
./sbox run -timeout 30s ./some-skill -- sh -c 'ls; python3 x.py'
```

`run` exits with the command's status, 124 on timeout, 125 when sbox fails.

Each run gets a fresh microVM, started by Firecracker's jailer as the
unprivileged `sbox-vm` user in its own chroot, cgroup and network namespace:

- the namespace holds only the run's tap device: the guest reaches the VM side
  (172.16.0.1) and nothing else, with no route out

- root filesystem read-only (shared image); scratch (`/tmp`, `/home/runner`) in tmpfs
- the directory is copied to `/home/runner/work`; the command runs there as `runner` (uid 1000)
- the guest's output streams live, with control characters escaped
- result markers carry a per-run nonce the command can't read, so it can't fake
  its exit status; the timeout is reported outside the guest's console
- the run's jail, drive and cgroup are removed afterwards, also on timeout

`setup` is idempotent. Every download is checked against a SHA-256 pinned in
`setup.go` (Firecracker v1.17.0, guest kernel 6.1.155, Node v24.21.0, uv 0.13.0);
the guest's Ubuntu packages come from the signed archive via mmdebstrap. Bump
`ROOTFS_REVISION` in `scripts/provision.sh` when the image recipe changes.

`doctor` boots a microVM to check the toolchain, then does a real `sbox run`
and requires that the command runs as uid 1000 on a read-only root, reaches
the VM side of its tap, **and cannot reach the internet**.

Needs: Apple Silicon M3 or later, macOS 15+, `brew install lima` (2.x).
The VM uses 4 CPUs and 6 GiB while running: `limactl stop sbox` frees them.
