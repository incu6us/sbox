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
cp -a /drive/skill /home/runner/work
chown -R runner:runner /home/runner

echo "SBOX-$nonce START"
cd /home/runner/work
setpriv --reuid=runner --regid=runner --init-groups --reset-env sh /drive/sbox/cmd.sh </dev/null 2>&1
status=$?
echo # the command's output may not end with a newline
echo "SBOX-$nonce EXIT $status"
sync
# No systemd, so no reboot(8): ask the kernel directly. Firecracker exits on reboot.
echo 1 >/proc/sys/kernel/sysrq
echo b >/proc/sysrq-trigger
