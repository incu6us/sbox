#!/usr/bin/env bash
# Doctor's checks inside the sbox VM. Prints "SBOX key=value" lines for the
# Go side to judge; never fails early, so every check reports.
set -uo pipefail
cd /opt/sbox 2>/dev/null || { echo "SBOX provisioned=no"; exit 0; }
echo "SBOX provisioned=yes"

[[ -c /dev/kvm ]] && echo "SBOX kvm=present" || echo "SBOX kvm=missing"

echo "SBOX firecracker=$(bin/firecracker --version 2>/dev/null | head -1 | awk '{print $2}')"
sum() { [[ -f "$1" ]] && sha256sum "$1" | cut -d' ' -f1; }
echo "SBOX firecracker-sha=$(sum cache/firecracker.tgz)"
echo "SBOX kernel-sha=$(sum vmlinux)"
[[ -f rootfs.ext4 ]] && echo "SBOX rootfs=present" || echo "SBOX rootfs=missing"
[[ -x bin/sbox-run ]] && id -u sbox-vm >/dev/null 2>&1 && echo "SBOX runner=present" || echo "SBOX runner=missing"
[[ -f rootfs.ext4 && -e /dev/kvm ]] || exit 0

# Boot test: a read-only microVM reporting its toolchain. Networking and the
# runner are checked separately, through a real `sbox run`.
cfg=$(mktemp) log=$(mktemp)
trap 'rm -f "$cfg" "$log"' EXIT

cat >"$cfg" <<EOF
{
  "boot-source": {
    "kernel_image_path": "/opt/sbox/vmlinux",
    "boot_args": "keep_bootcon console=ttyS0 reboot=k panic=1 ro init=/sbin/sbox-hello"
  },
  "drives": [{"drive_id": "rootfs", "path_on_host": "/opt/sbox/rootfs.ext4", "is_root_device": true, "is_read_only": true}],
  "machine-config": {"vcpu_count": 1, "mem_size_mib": 512}
}
EOF

start=$(date +%s%N)
# As root, like the jailer that starts real runs. </dev/null: the serial
# console reads stdin and would swallow this script.
sudo timeout 60 bin/firecracker --no-api --config-file "$cfg" </dev/null >"$log" 2>&1
echo "SBOX boot-exit=$?"
echo "SBOX boot-ms=$((($(date +%s%N) - start) / 1000000))"
grep -a '^SBOX ' "$log"
exit 0 # failures are reported as values, judged on the Go side
