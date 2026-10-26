# sbox

Runs AI agent skills inside a Firecracker microVM with fake secrets and a
fake internet, and reports what they try to do. Work in progress: milestone 1
of the plan (VM, Firecracker, guest image, doctor) is done.

```
macOS ── limactl ──▶ Lima VM "sbox" (vz, nested virtualization, no host mounts)
                     └─ Firecracker ──▶ microVM per run (node, python, uv, strace)
```

```sh
go build -o sbox .
./sbox setup    # create + provision the VM, then run doctor (first run ~1 min)
./sbox doctor   # ~7 s: VM, KVM, pinned artifacts, microVM boot, network boundary
```

`setup` is idempotent. Every download is checked against a SHA-256 pinned in
`setup.go` (Firecracker v1.17.0, guest kernel 6.1.155, Node v24.21.0, uv 0.13.0);
the guest's Ubuntu packages come from the signed archive via mmdebstrap. Bump
`ROOTFS_REVISION` in `scripts/provision.sh` when the image recipe changes.

`doctor` boots a read-only microVM on a tap device and requires that the guest
reaches the VM side of the tap **and cannot reach the internet**.

Needs: Apple Silicon M3 or later, macOS 15+, `brew install lima` (2.x).
The VM uses 4 CPUs and 6 GiB while running: `limactl stop sbox` frees them.
