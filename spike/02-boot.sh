#!/usr/bin/env bash
# Spike step 2: boot a Firecracker microVM with a hello init and time it.
# Runs inside the sbox VM after 01-install.sh.
set -euo pipefail
cd /opt/sbox

command -v unsquashfs >/dev/null || sudo apt-get install -y -qq squashfs-tools >/dev/null

if [[ ! -f rootfs.ext4 ]]; then
	sudo rm -rf rootfs && sudo unsquashfs -q -d rootfs rootfs.squashfs
	sudo tee rootfs/sbox-hello >/dev/null <<-'EOF'
	#!/bin/sh
	mount -t proc proc /proc
	echo "SBOX-HELLO uname=$(uname -r) cpus=$(nproc) mem=$(awk '/MemTotal/{print $2}' /proc/meminfo)kB"
	echo "SBOX-HELLO virt=$(cat /sys/firmware/devicetree/base/compatible 2>/dev/null | tr '\0' ' ')"
	echo "SBOX-HELLO node=$(command -v node || echo none) python=$(command -v python3 || echo none) strace=$(command -v strace || echo none)"
	reboot -f
	EOF
	sudo chmod +x rootfs/sbox-hello
	truncate -s 2G rootfs.ext4
	sudo mkfs.ext4 -q -d rootfs -F rootfs.ext4
fi

cat >vm.json <<EOF
{
  "boot-source": {
    "kernel_image_path": "/opt/sbox/vmlinux",
    "boot_args": "keep_bootcon console=ttyS0 reboot=k panic=1 init=/sbox-hello"
  },
  "drives": [{ "drive_id": "rootfs", "path_on_host": "/opt/sbox/rootfs.ext4", "is_root_device": true, "is_read_only": false }],
  "machine-config": { "vcpu_count": 2, "mem_size_mib": 1024 }
}
EOF

rm -f /tmp/fc.sock
start=$(date +%s.%N)
# </dev/null: the serial console reads stdin and would swallow the rest of a piped script.
timeout 60 bin/firecracker --no-api --config-file vm.json --api-sock /tmp/fc.sock </dev/null >boot.log 2>&1 || true
end=$(date +%s.%N)
grep -E 'SBOX-HELLO|Kernel panic|Firecracker exiting' boot.log | head -20 || true
printf 'boot-to-exit: %.2fs\n' "$(echo "$end - $start" | bc)"
