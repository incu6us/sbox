#!/usr/bin/env bash
# Runs one microVM. Reads the run as a tar on stdin (skill/ and sbox/), builds
# its drive, boots Firecracker under the jailer as an unprivileged user,
# streams the serial console to stdout, and removes everything on exit.
#
# Network: each run gets its own network namespace holding only loopback and
# the run's tap device. There is no veth and no route out, so the guest can
# reach the VM side of its tap (172.16.0.1) and nothing else.
# Usage (as root): sbox-run <run-id> <timeout-seconds>
set -euo pipefail

id=${1:-} timeout=${2:-}
[[ $id =~ ^[a-f0-9]{8,32}$ ]] || { echo "sbox-run: bad run id" >&2; exit 2; }
[[ $timeout =~ ^[0-9]{1,5}$ ]] || { echo "sbox-run: bad timeout" >&2; exit 2; }

base=/opt/sbox
jail_base=/srv/sbox-jail
run=/var/lib/sbox/runs/$id
root=$jail_base/firecracker/$id/root
netns=sbox-$id

cleanup() {
	ip netns del "$netns" 2>/dev/null || true
	rm -rf "$run" "$jail_base/firecracker/$id"
	rmdir "/sys/fs/cgroup/firecracker/$id" 2>/dev/null || true
}
trap cleanup EXIT

mkdir -p "$run/drive" "$root"
tar -x -C "$run/drive" --no-same-owner --no-same-permissions

# The run's drive: the skill plus the guest init and command, read-only in the guest.
size_mb=$(($(du -sm "$run/drive" | cut -f1) * 13 / 10 + 32))
truncate -s "${size_mb}M" "$root/drive.ext4"
mkfs.ext4 -q -d "$run/drive" -F "$root/drive.ext4"

# Same filesystem, so hard links: no copy of the kernel or the 800 MB rootfs.
ln "$base/vmlinux" "$root/vmlinux"
ln "$base/rootfs.ext4" "$root/rootfs.ext4"

ip netns add "$netns"
in_ns() { ip netns exec "$netns" "$@"; }
in_ns ip link set lo up
in_ns ip tuntap add dev tap0 mode tap user "$(id -u sbox-vm)"
in_ns ip addr add 172.16.0.1/30 dev tap0
in_ns ip link set tap0 up

cat >"$root/vm.json" <<'EOF'
{
  "boot-source": {
    "kernel_image_path": "/vmlinux",
    "boot_args": "console=ttyS0 reboot=k panic=1 quiet loglevel=1 ro init=/sbin/sbox-init"
  },
  "drives": [
    {"drive_id": "rootfs", "path_on_host": "/rootfs.ext4", "is_root_device": true, "is_read_only": true},
    {"drive_id": "run", "path_on_host": "/drive.ext4", "is_root_device": false, "is_read_only": true}
  ],
  "network-interfaces": [{"iface_id": "eth0", "host_dev_name": "tap0", "guest_mac": "06:00:ac:10:00:02"}],
  "machine-config": {"vcpu_count": 2, "mem_size_mib": 1024}
}
EOF

# Exit 124 on timeout, 0 otherwise: the console (stdout) belongs to the
# guest, so the timeout is reported out of its reach.
status=0
# </dev/null: the serial console reads stdin.
timeout --kill-after=5 "$timeout" "$base/bin/jailer" \
	--id "$id" \
	--exec-file "$base/bin/firecracker" \
	--uid "$(id -u sbox-vm)" --gid "$(id -g sbox-vm)" \
	--chroot-base-dir "$jail_base" \
	--cgroup-version 2 \
	--netns "/var/run/netns/$netns" \
	-- --no-api --config-file /vm.json </dev/null || status=$?
if [[ $status == 124 || $status == 137 ]]; then
	exit 124
fi
exit 0
