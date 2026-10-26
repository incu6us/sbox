#!/bin/sh
# Per-run guest init, started by /sbin/sbox-init from the run's drive.
# The root filesystem is read-only: writable scratch lives in tmpfs. The
# command runs as the unprivileged runner user, then the VM powers off.
# Markers carry the run's nonce, readable by root only, so the command
# can't print a fake result.
nonce=$(cat /drive/sbox/nonce)
for d in /tmp /var/tmp /run /home/runner; do
	mount -t tmpfs -o mode=1777,size=256m tmpfs "$d"
done
chmod 0755 /home/runner

# Network: the VM side of the tap is the only thing reachable.
ip link set lo up
if ip link show eth0 >/dev/null 2>&1; then
	ip addr add 172.16.0.2/30 dev eth0
	ip link set eth0 up
	ip route add default via 172.16.0.1
fi
# Unprivileged ICMP sockets, so the runner can ping (normally set by systemd).
echo "0 2147483647" >/proc/sys/net/ipv4/ping_group_range
cp -a /drive/skill /home/runner/work
chown -R runner:runner /home/runner

echo "SBOX-$nonce START"
cd /home/runner/work
if [ -f /drive/sbox/scan.sh ]; then
	# Scan mode: the script runs as root and drops each entry point to the
	# runner under strace. It prints its own markers, so it gets the nonce.
	nonce="$nonce" sh /drive/sbox/scan.sh </dev/null 2>&1
else
	setpriv --reuid=runner --regid=runner --init-groups --reset-env sh /drive/sbox/cmd.sh </dev/null 2>&1
fi
status=$?
echo # the command's output may not end with a newline
echo "SBOX-$nonce EXIT $status"
sync
# No systemd, so no reboot(8): ask the kernel directly. Firecracker exits on reboot.
echo 1 >/proc/sys/kernel/sysrq
echo b >/proc/sysrq-trigger
